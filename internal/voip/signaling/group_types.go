package signaling

import (
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

// CallControlEnvelope is the routing metadata and sole action in a raw call node.
// The whatsmeow client only types offer/accept/preaccept/transport/terminate/
// reject/relaylatency; every group control action arrives as an unknown call
// event carrying the raw node, and this is how it is read.
type CallControlEnvelope struct {
	From        types.JID
	Participant types.JID
	Recipient   types.JID
	Timestamp   time.Time
	CallID      string
	CallCreator types.JID
	Action      waBinary.Node
}

// GroupCallUpdate is one authoritative group-call roster and relay snapshot.
type GroupCallUpdate struct {
	CallID         string
	CallCreator    types.JID
	GroupJID       types.JID
	TransactionID  uint32
	Media          string
	ConnectedLimit uint32
	Joinable       bool
	AVUpgradable   bool
	RekeyRequested bool
	Participants   []GroupCallParticipant
	Relay          *GroupCallRelay
}

// GroupCallParticipant is one user in a group-call roster.
type GroupCallParticipant struct {
	JID     types.JID
	PN      types.JID
	State   string
	Type    string
	Devices []GroupCallDevice
}

// GroupCallDevice is one participant device in a group-call roster. Capability is
// echoed back verbatim in the accept, so it is cloned rather than referenced.
type GroupCallDevice struct {
	JID               types.JID
	Platform          string
	PID               uint32
	HasPID            bool
	CapabilityVersion uint32
	Capability        []byte
}

// GroupCallRelay describes the shared relay allocated to a group call. It is
// richer than the 1:1 relay block: the tokens are indexed, and the endpoints
// carry the participant id this device was assigned.
type GroupCallRelay struct {
	TransactionID      uint32
	SelfPID            uint32
	HasSelfPID         bool
	UUID               string
	ParticipantUUID    string
	AttributePadding   bool
	WarpMITagLength    uint32
	HasWarpMITagLength bool
	Key                []byte
	HBHKey             []byte
	Tokens             [][]byte
	AuthTokens         [][]byte
	Endpoints          []GroupCallRelayEndpoint
}

// GroupCallRelayEndpoint is one address record in a group relay allocation.
type GroupCallRelayEndpoint struct {
	RelayID     uint32
	TokenID     uint32
	AuthTokenID uint32
	RelayName   string
	DomainName  string
	RTT         uint32
	IsFNA       bool
	Address     []byte
	IPv4        string
	Port        uint16
}

// GroupCallEncRekey is one encrypted shared-key epoch delivered to this device.
// The group media plane keys off this epoch, not off the 1:1 call key.
type GroupCallEncRekey struct {
	CallID            string
	CallCreator       types.JID
	TransactionID     uint32
	KeyGeneration     uint32
	EncryptionType    string
	EncryptionVersion uint32
	Ciphertext        []byte
}
