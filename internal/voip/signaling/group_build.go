package signaling

import (
	"bytes"
	"fmt"
	"strconv"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

// callServiceJID is where group and call-link stanzas are addressed: the call id
// on the "call" service, not a peer device.
func callServiceJID(callID string) types.JID { return types.NewJID(callID, "call") }

func audioOpusNode(rate string) waBinary.Node {
	return waBinary.Node{Tag: "audio", Attrs: waBinary.Attrs{"enc": "opus", "rate": rate}}
}

func destinationNode(devices []types.JID) waBinary.Node {
	tos := make([]waBinary.Node, len(devices))
	for i, device := range devices {
		tos[i] = waBinary.Node{Tag: "to", Attrs: waBinary.Attrs{"jid": device}}
	}
	return waBinary.Node{Tag: "destination", Content: tos}
}

func offerActionNode(tag, callID string, callCreator types.JID, children []waBinary.Node) waBinary.Node {
	return waBinary.Node{
		Tag:     tag,
		Attrs:   waBinary.Attrs{"call-id": callID, "call-creator": callCreator},
		Content: children,
	}
}

// GroupOfferDeviceKey is one device's encrypted copy of a shared key epoch.
type GroupOfferDeviceKey struct {
	DeviceJID  types.JID
	EncType    string
	Ciphertext []byte
}

// InitialGroupOfferParams contains the wire inputs for starting a group call.
type InitialGroupOfferParams struct {
	CallID       string
	CallCreator  types.JID
	GroupJID     types.JID
	Participants []GroupCallParticipant
}

// BuildInitialGroupOffer builds an ad-hoc or group-bound initial group offer.
//
// The child order is load-bearing: the server rejects the stanza with 439 when it
// is wrong. Audio only, because this branch carries no video path.
func BuildInitialGroupOffer(params InitialGroupOfferParams) (waBinary.Node, error) {
	if params.CallID == "" {
		return waBinary.Node{}, fmt.Errorf("signaling: build initial group offer: call ID is required")
	}
	if params.CallCreator.IsEmpty() {
		return waBinary.Node{}, fmt.Errorf("signaling: build initial group offer: call creator is required")
	}
	// The captured client never opens a group call with fewer than three: self
	// plus two remote participants.
	if len(params.Participants) < 3 {
		return waBinary.Node{}, fmt.Errorf(
			"signaling: build initial group offer: self and at least two remote participants are required")
	}
	users, err := buildGroupUsers(params.Participants)
	if err != nil {
		return waBinary.Node{}, err
	}
	children := []waBinary.Node{
		audioOpusNode("8000"),
		audioOpusNode("16000"),
		{Tag: "net", Attrs: waBinary.Attrs{"medium": "3"}},
		{Tag: "group_info", Content: users},
	}
	offer := offerActionNode("offer", params.CallID, params.CallCreator, children)
	if !params.GroupJID.IsEmpty() {
		offer.Attrs["group-jid"] = params.GroupJID
	}
	return callWrap(callServiceJID(params.CallID), offer), nil
}

// GroupInviteOfferParams contains the wire inputs for inviting or ringing one
// participant into a call that is already active.
type GroupInviteOfferParams struct {
	CallID        string
	To            types.JID
	CallCreator   types.JID
	TargetDevices []types.JID
	Participants  []GroupCallParticipant
}

// BuildGroupInviteOffer builds a singular active-call participant offer.
func BuildGroupInviteOffer(params GroupInviteOfferParams) (waBinary.Node, error) {
	if params.CallID == "" || params.To.IsEmpty() || params.CallCreator.IsEmpty() {
		return waBinary.Node{}, fmt.Errorf(
			"signaling: build group invite offer: call identity and target are required")
	}
	if len(params.TargetDevices) == 0 || len(params.Participants) == 0 {
		return waBinary.Node{}, fmt.Errorf(
			"signaling: build group invite offer: target devices and roster are required")
	}
	users, err := buildGroupUsers(params.Participants)
	if err != nil {
		return waBinary.Node{}, err
	}
	children := []waBinary.Node{
		audioOpusNode("16000"),
		{Tag: "net", Attrs: waBinary.Attrs{"medium": "2"}},
		destinationNode(params.TargetDevices),
		{Tag: "group_info", Content: users},
	}
	return callWrap(params.To, offerActionNode("offer", params.CallID, params.CallCreator, children)), nil
}

// BuildActiveGroupPreaccept builds the eager response to an active-call invite.
func BuildActiveGroupPreaccept(callID string, callCreator types.JID, requestID string) (waBinary.Node, error) {
	if callID == "" || callCreator.IsEmpty() || requestID == "" {
		return waBinary.Node{}, fmt.Errorf("signaling: build active group preaccept: incomplete identity")
	}
	action := offerActionNode("preaccept", callID, callCreator, []waBinary.Node{
		audioOpusNode("16000"),
		{Tag: "encopt", Attrs: waBinary.Attrs{"keygen": "2"}},
		{Tag: "capability", Attrs: waBinary.Attrs{"ver": "1"}, Content: capabilityPreaccept},
	})
	return callWrapWithID(callServiceJID(callID), requestID, action), nil
}

// BuildActiveGroupAccept builds the immediate acceptance of an active-call invite.
// Unlike the 1:1 accept it carries no encrypted call key: a group call keys off
// the shared epoch delivered separately by enc_rekey.
func BuildActiveGroupAccept(callID string, callCreator types.JID, requestID string) (waBinary.Node, error) {
	if callID == "" || callCreator.IsEmpty() || requestID == "" {
		return waBinary.Node{}, fmt.Errorf("signaling: build active group accept: incomplete identity")
	}
	action := offerActionNode("accept", callID, callCreator, []waBinary.Node{
		audioOpusNode("16000"),
		{Tag: "encopt", Attrs: waBinary.Attrs{"keygen": "2"}},
		{Tag: "capability", Attrs: waBinary.Attrs{"ver": "1"}, Content: capabilityOffer},
	})
	return callWrapWithID(callServiceJID(callID), requestID, action), nil
}

// GroupEncRekeyParams contains one direct shared-key epoch delivery.
type GroupEncRekeyParams struct {
	CallID        string
	To            types.JID
	CallCreator   types.JID
	TransactionID uint32
	RequestID     string
	DeviceKey     GroupOfferDeviceKey
}

// BuildGroupEncRekey builds one keygen-v2 group epoch stanza for one device.
func BuildGroupEncRekey(params GroupEncRekeyParams) (waBinary.Node, error) {
	if params.CallID == "" || params.To.IsEmpty() || params.CallCreator.IsEmpty() ||
		params.TransactionID == 0 || params.RequestID == "" {
		return waBinary.Node{}, fmt.Errorf("signaling: build group rekey: incomplete identity")
	}
	if params.DeviceKey.DeviceJID != params.To || len(params.DeviceKey.Ciphertext) == 0 {
		return waBinary.Node{}, fmt.Errorf(
			"signaling: build group rekey: encrypted device mismatch or empty ciphertext")
	}
	if params.DeviceKey.EncType != "msg" && params.DeviceKey.EncType != "pkmsg" {
		return waBinary.Node{}, fmt.Errorf(
			"signaling: build group rekey: unsupported encryption type %q", params.DeviceKey.EncType)
	}
	action := waBinary.Node{
		Tag: "enc_rekey",
		Attrs: waBinary.Attrs{
			"call-id": params.CallID, "call-creator": params.CallCreator,
			"transaction-id": strconv.FormatUint(uint64(params.TransactionID), 10),
		},
		Content: []waBinary.Node{
			{Tag: "encopt", Attrs: waBinary.Attrs{"keygen": "2"}},
			{
				Tag:     "enc",
				Attrs:   waBinary.Attrs{"v": "2", "type": params.DeviceKey.EncType, "count": "0"},
				Content: bytes.Clone(params.DeviceKey.Ciphertext),
			},
		},
	}
	return callWrapWithID(params.To, params.RequestID, action), nil
}

func buildGroupUsers(participants []GroupCallParticipant) ([]waBinary.Node, error) {
	users := make([]waBinary.Node, len(participants))
	for participantIndex, participant := range participants {
		if participant.JID.IsEmpty() || len(participant.Devices) == 0 {
			return nil, fmt.Errorf("signaling: build group users: participant %d is incomplete", participantIndex)
		}
		userAttrs := waBinary.Attrs{"jid": participant.JID}
		if participant.State != "" {
			userAttrs["state"] = participant.State
		}
		devices := make([]waBinary.Node, len(participant.Devices))
		for deviceIndex, device := range participant.Devices {
			if device.JID.IsEmpty() {
				return nil, fmt.Errorf(
					"signaling: build group users: participant %d device %d JID is required",
					participantIndex, deviceIndex)
			}
			var content []waBinary.Node
			// The capability is echoed exactly as the roster reported it: it is the
			// server's own value for that device, not something to reconstruct.
			if device.Capability != nil {
				attrs := make(waBinary.Attrs)
				if device.CapabilityVersion != 0 {
					attrs["ver"] = strconv.FormatUint(uint64(device.CapabilityVersion), 10)
				}
				content = []waBinary.Node{
					{Tag: "capability", Attrs: attrs, Content: bytes.Clone(device.Capability)},
				}
			}
			devices[deviceIndex] = waBinary.Node{
				Tag: "device", Attrs: waBinary.Attrs{"jid": device.JID}, Content: content,
			}
		}
		users[participantIndex] = waBinary.Node{Tag: "user", Attrs: userAttrs, Content: devices}
	}
	return users, nil
}
