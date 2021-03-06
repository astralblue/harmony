package newnode

import (
	"time"

	"github.com/harmony-one/harmony/api/proto"
	proto_identity "github.com/harmony-one/harmony/api/proto/identity"
	"github.com/harmony-one/harmony/p2p"
)

// NodeHandler handles a new incoming connection.
func (node *NewNode) NodeHandler(s p2p.Stream) {
	defer s.Close()
	defer node.host.Close()
	s.SetReadDeadline(time.Now().Add(1 * time.Second)) // This deadline is for 1 second to accept new connections.
	content, err := p2p.ReadMessageContent(s)
	if err != nil {
		node.log.Error("Read p2p data failed", "err", err, "node", node)
		return
	}

	msgCategory, err := proto.GetMessageCategory(content)
	if err != nil {
		node.log.Error("Read node type failed", "err", err, "node", node)
		return
	}

	msgType, err := proto.GetMessageType(content)
	if err != nil {
		node.log.Error("Read action type failed", "err", err, "node", node)
		return
	}

	msgPayload, err := proto.GetMessagePayload(content)
	if err != nil {
		node.log.Error("Read message payload failed", "err", err, "node", node)
		return
	}
	identityMsgPayload, err := proto_identity.GetIdentityMessagePayload(msgPayload)
	if err != nil {
		node.log.Error("Read message payload failed")
		return
	}
	switch msgCategory {
	case proto.Identity:
		actionType := proto_identity.IDMessageType(msgType)
		switch actionType {
		case proto_identity.Identity:
			idMsgType, err := proto_identity.GetIdentityMessageType(msgPayload)
			if err != nil {
				node.log.Error("Error finding the identity message type", err)
			}
			switch idMsgType {
			case proto_identity.Acknowledge:
				node.processShardInfo(identityMsgPayload)
			default:
				panic("The identity message type is wrong/missing and newnode does not handle this identity message type")
			}
		default:
			panic("The msgCategory is wrong/missing and newnode does not handle this protocol message type")
		}
	}
}
