package messages

import (
	"sync"

	"github.com/TeamFoxx2025/go-ibft/messages/proto"
)

// Messages contains the relevant messages for each view (height, round)
type Messages struct {
	// manager for incoming message events
	eventManager *eventManager

	// mutex map that protects different message type queues
	muxMap map[proto.MessageType]*sync.RWMutex

	// message maps for different message types
	preprepareMessages,
	prepareMessages,
	commitMessages,
	roundChangeMessages heightMessageMap
}

// Subscribe creates a new message type subscription
func (ms *Messages) Subscribe(details SubscriptionDetails) *Subscription {
	return ms.eventManager.subscribe(details)
}

// Unsubscribe cancels a message type subscription
func (ms *Messages) Unsubscribe(id SubscriptionID) {
	ms.eventManager.cancelSubscription(id)
}

// NewMessages returns a new Messages wrapper
func NewMessages() *Messages {
	return &Messages{
		preprepareMessages:  make(heightMessageMap),
		prepareMessages:     make(heightMessageMap),
		commitMessages:      make(heightMessageMap),
		roundChangeMessages: make(heightMessageMap),

		eventManager: newEventManager(),

		muxMap: map[proto.MessageType]*sync.RWMutex{
			proto.MessageType_PREPREPARE:   {},
			proto.MessageType_PREPARE:      {},
			proto.MessageType_COMMIT:       {},
			proto.MessageType_ROUND_CHANGE: {},
		},
	}
}

// AddMessage adds a new message to the message queue
func (ms *Messages) AddMessage(message *proto.IbftMessage) {
	mux := ms.muxMap[message.Type]
	mux.Lock()
	defer mux.Unlock()

	// Get the corresponding height map
	heightMsgMap := ms.getMessageMap(message.Type)

	// Append the message to the appropriate queue
	messages := heightMsgMap.getViewMessages(message.View)
	messages[string(message.From)] = message
}

// SignalEvent signals event
func (ms *Messages) SignalEvent(messageType proto.MessageType, view *proto.View) {
	ms.eventManager.signalEvent(messageType, &proto.View{
		Height: view.Height,
		Round:  view.Round})
}

// Close closes event manager
func (ms *Messages) Close() {
	ms.eventManager.close()
}

// getMessageMap fetches the corresponding message map by type
func (ms *Messages) getMessageMap(messageType proto.MessageType) heightMessageMap {
	switch messageType {
	case proto.MessageType_PREPREPARE:
		return ms.preprepareMessages
	case proto.MessageType_PREPARE:
		return ms.prepareMessages
	case proto.MessageType_COMMIT:
		return ms.commitMessages
	case proto.MessageType_ROUND_CHANGE:
		return ms.roundChangeMessages
	}

	return nil
}

// numMessages returns the number of messages received for the specific type
func (ms *Messages) numMessages(
	view *proto.View,
	messageType proto.MessageType,
) int {
	mux := ms.muxMap[messageType]
	mux.RLock()
	defer mux.RUnlock()

	heightMsgMap := ms.getMessageMap(messageType)

	// Check if the round map is present
	roundMsgMap, found := heightMsgMap[view.Height]
	if !found {
		return 0
	}

	// Check if the messages array is present
	messages, found := roundMsgMap[view.Round]
	if !found {
		return 0
	}

	return len(messages)
}

// PruneByHeight prunes out all old messages from the message queues
// by the specified height in the view
func (ms *Messages) PruneByHeight(height uint64) {
	possibleMaps := []proto.MessageType{
		proto.MessageType_PREPREPARE,
		proto.MessageType_PREPARE,
		proto.MessageType_COMMIT,
		proto.MessageType_ROUND_CHANGE,
	}

	// Prune out the views from all possible message types
	for _, messageType := range possibleMaps {
		mux := ms.muxMap[messageType]
		mux.Lock()

		messageMap := ms.getMessageMap(messageType)

		// Delete all height maps up until the specified
		// view height
		for msgHeight := range messageMap {
			if msgHeight < height {
				delete(messageMap, msgHeight)
			}
		}

		mux.Unlock()
	}
}

// getProtoMessages fetches the underlying proto messages for the specified view
// and message type
func (ms *Messages) getProtoMessages(
	view *proto.View,
	messageType proto.MessageType,
) protoMessages {
	heightMsgMap := ms.getMessageMap(messageType)

	// Check if the round map is present
	roundMsgMap, found := heightMsgMap[view.Height]
	if !found {
		return nil
	}

	return roundMsgMap[view.Round]
}

// GetValidMessages fetches all messages of a specific type for the specified view,
// that pass the validity check; invalid messages are pruned out
func (ms *Messages) GetValidMessages(
	view *proto.View,
	messageType proto.MessageType,
	isValid func(message *proto.IbftMessage) bool,
) []*proto.IbftMessage {
	mux := ms.muxMap[messageType]
	mux.Lock()
	defer mux.Unlock()

	validMessages := make([]*proto.IbftMessage, 0)

	invalidMessageKeys := make([]string, 0)
	messages := ms.getProtoMessages(view, messageType)

	for key, message := range messages {
		if !isValid(message) {
			invalidMessageKeys = append(invalidMessageKeys, key)

			continue
		}

		validMessages = append(validMessages, message)
	}

	// Prune out invalid messages
	for _, key := range invalidMessageKeys {
		delete(messages, key)
	}

	return validMessages
}

// GetExtendedRCC returns Round-Change-Certificate for the highest round
func (ms *Messages) GetExtendedRCC(
	height uint64,
	isValidMessage func(message *proto.IbftMessage) bool,
	isValidRCC func(round uint64, messages []*proto.IbftMessage) bool,
) []*proto.IbftMessage {
	messageType := proto.MessageType_ROUND_CHANGE

	mux := ms.muxMap[messageType]
	mux.Lock()
	defer mux.Unlock()

	// Get all ROUND-CHANGE messages for the height
	roundMessageMap := ms.getMessageMap(messageType)[height]

	var (
		highestRound uint64
		extendedRCC  []*proto.IbftMessage
	)

	for round, messages := range roundMessageMap {
		validMessages := make([]*proto.IbftMessage, 0, len(messages))

		if round <= highestRound {
			continue
		}

		for _, msg := range messages {
			if !isValidMessage(msg) {
				continue
			}

			validMessages = append(validMessages, msg)
		}

		if !isValidRCC(round, validMessages) {
			continue
		}

		highestRound = round
		extendedRCC = validMessages
	}

	return extendedRCC
}

// GetMostRoundChangeMessages fetches most round change messages
// for the minimum round and above
func (ms *Messages) GetMostRoundChangeMessages(minRound, height uint64) []*proto.IbftMessage {
	messageType := proto.MessageType_ROUND_CHANGE

	mux := ms.muxMap[messageType]
	mux.RLock()
	defer mux.RUnlock()

	roundMessageMap := ms.getMessageMap(messageType)[height]

	var (
		bestRound              = uint64(0)
		bestRoundMessagesCount = 0
	)

	for round, msgs := range roundMessageMap {
		if round < minRound {
			continue
		}

		size := len(msgs)
		if size > bestRoundMessagesCount {
			bestRound = round
			bestRoundMessagesCount = size
		}
	}

	if bestRound == 0 {
		//	no messages found
		return nil
	}

	messages := make([]*proto.IbftMessage, 0, bestRoundMessagesCount)
	for _, msg := range roundMessageMap[bestRound] {
		messages = append(messages, msg)
	}

	return messages
}

// heightMessageMap maps the height number -> round message map
type heightMessageMap map[uint64]roundMessageMap

// roundMessageMap maps the round number -> messages
type roundMessageMap map[uint64]protoMessages

// protoMessages is the set of messages that circulate.
// It contains a mapping between the sender and their messages to avoid duplicates
type protoMessages map[string]*proto.IbftMessage

// getViewMessages fetches the message queue for the specified view (height + round).
// It will initialize a new message array if it's not found
func (m heightMessageMap) getViewMessages(view *proto.View) protoMessages {
	var (
		height = view.Height
		round  = view.Round
	)

	// Check if the height is present
	roundMessages, exists := m[height]
	if !exists {
		roundMessages = roundMessageMap{}

		m[height] = roundMessages
	}

	// Check if the round is present
	messages, exists := roundMessages[round]
	if !exists {
		messages = protoMessages{}

		roundMessages[round] = messages
	}

	return messages
}
