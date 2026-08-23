package call

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
	"wacalls/internal/voip/signaling"
	"wacalls/internal/voip/transport"
	"wacalls/internal/voip/wanode"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

// GroupState is what a call knows about a group beyond the 1:1 state: who is on
// it and the shared key their media is encrypted with. It stays nil on a 1:1
// call, so nothing on that path changes.
type GroupState struct {
	Roster        *signaling.GroupCallUpdate
	Epoch         []byte
	TransactionID uint32
}

// GroupState returns a snapshot of the group state, or nil on a 1:1 call.
func (m *CallManager) GroupState() *GroupState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.group == nil {
		return nil
	}
	snapshot := *m.group
	return &snapshot
}

// HandleControl ingests one raw group control node. Whatsmeow does not type these,
// so they arrive as unknown call events carrying the node.
//
// The ack is sent for every control action, including the ones this does not act
// on yet: a missing or untyped ack makes the peer revert the state it announced.
func (m *CallManager) HandleControl(ctx context.Context, node *waBinary.Node) {
	envelope, err := signaling.ParseCallControlEnvelope(node)
	if err != nil {
		return
	}
	m.mu.Lock()
	call := m.currentCall
	live := call != nil && call.CallID == envelope.CallID && !call.IsEnded()
	m.mu.Unlock()
	if !live {
		return
	}

	// Sent synchronously and through SendNode, not Query: an ack has no response
	// to correlate, and the peer reverts what it announced if it does not arrive.
	if ack, ok := signaling.BuildCallControlAck(node, envelope.Action.Tag); ok {
		if err := m.sock.SendNode(ctx, ack); err != nil {
			m.log.Warn("control ack failed to send",
				"call_id", envelope.CallID, "type", envelope.Action.Tag, "err", err)
		}
	}

	switch envelope.Action.Tag {
	case "group_update":
		m.applyGroupUpdate(&envelope.Action)
	case "enc_rekey":
		m.applyGroupEpoch(ctx, envelope)
	}
}

func (m *CallManager) applyGroupUpdate(action *waBinary.Node) {
	update, err := signaling.ParseGroupUpdate(action)
	if err != nil {
		m.log.Warn("group update rejected", "call_id", m.callIDForLog(), "err", err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// The server resends snapshots, and applying an older one reinstates
	// participants that already left.
	if m.group != nil && update.TransactionID <= m.group.TransactionID {
		m.log.Debug("group update ignored as stale",
			"call_id", update.CallID,
			"transaction_id", update.TransactionID,
			"current", m.group.TransactionID)
		return
	}
	if m.group == nil {
		m.group = &GroupState{}
	}
	m.group.Roster = update
	m.group.TransactionID = update.TransactionID
	m.syncGroupRecvKeysLocked()
	m.log.Info("group roster applied",
		"call_id", update.CallID,
		"transaction_id", update.TransactionID,
		"participants", len(update.Participants),
		"devices", countRosterDevices(update),
		// Whether the server lists us, and with what state, is what decides if the
		// other participants can see us at all.
		"self_in_roster", m.selfInRosterLocked(update),
		"rekey_requested", update.RekeyRequested,
		"roster", describeRosterLocked(update))
}

func (m *CallManager) applyGroupEpoch(ctx context.Context, envelope *signaling.CallControlEnvelope) {
	rekey, err := signaling.ParseGroupCallEncRekey(&envelope.Action)
	if err != nil {
		m.log.Warn("group rekey rejected", "call_id", envelope.CallID, "err", err)
		return
	}
	enc, ok := envelope.Action.GetOptionalChildByTag("enc")
	if !ok {
		return
	}
	from := envelope.Participant
	if from.IsEmpty() {
		from = envelope.From
	}
	// The group epoch travels in the same Signal envelope as the 1:1 call key,
	// so the existing decrypt path answers for it unchanged.
	epoch, err := m.sock.DecryptCallKey(ctx, from, &enc)
	if err != nil || len(epoch) == 0 {
		m.log.Warn("group epoch could not be decrypted",
			"call_id", envelope.CallID, "transaction_id", rekey.TransactionID, "err", err)
		return
	}
	m.mu.Lock()
	if m.group == nil {
		m.group = &GroupState{}
	}
	m.group.Epoch = epoch
	m.syncGroupRecvKeysLocked()
	m.mu.Unlock()
	m.log.Info("group epoch installed",
		"call_id", envelope.CallID, "transaction_id", rekey.TransactionID, "bytes", len(epoch))
}

func (m *CallManager) callIDForLog() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.currentCall == nil {
		return ""
	}
	return m.currentCall.CallID
}

func countRosterDevices(update *signaling.GroupCallUpdate) int {
	n := 0
	for _, p := range update.Participants {
		n += len(p.Devices)
	}
	return n
}

// syncGroupRecvKeysLocked registers one SRTP receive key per remote participant
// device, so audio from every one of them authenticates. It runs whenever any of
// its three inputs lands, because the roster, the epoch and the SRTP session
// arrive in no fixed order.
//
// Only receive keys: the send key still comes from the 1:1 handshake. Until the
// group allocate exists nothing is sent on a group stream anyway.
//
// Caller holds m.mu.
func (m *CallManager) syncGroupRecvKeysLocked() {
	if m.group == nil || m.group.Roster == nil || len(m.group.Epoch) == 0 || m.srtp == nil {
		return
	}
	call := m.currentCall
	if call == nil {
		return
	}
	ourBase := wanode.CleanJID(m.ownCredJid())
	registered := 0
	for _, participant := range m.group.Roster.Participants {
		for _, device := range participant.Devices {
			if device.JID.IsEmpty() {
				continue
			}
			raw := device.JID.String()
			// Registering our own device as a receive key would make the relay
			// echo of our own stream authenticate as another participant.
			if wanode.CleanJID(raw) == ourBase {
				continue
			}
			deviceJID := ensureDeviceJid(raw)
			keying, err := media.DerivePerJidSrtpKey(m.group.Epoch, deviceJID)
			if err != nil {
				m.log.Warn("participant key derivation failed",
					"call_id", call.CallID, "device", deviceJID, "err", err)
				continue
			}
			ssrc := media.GenerateSecureSsrc(call.CallID, deviceJID, core.SsrcCounterAudio)
			m.srtp.SetRecvKeyForSSRC(ssrc, keying)
			registered++
		}
	}
	m.applyGroupSendKeyLocked()
	m.applyGroupAllocateLocked()
	if registered > 0 {
		m.log.Info("participant receive keys registered",
			"call_id", call.CallID,
			"transaction_id", m.group.TransactionID,
			"devices", registered)
	}
}

// applyGroupSendKeyLocked switches outbound media to the shared epoch. A group
// call is not encrypted with the 1:1 call key, so without this nobody can decode
// what we send. Caller holds m.mu.
func (m *CallManager) applyGroupSendKeyLocked() {
	call := m.currentCall
	if call == nil || m.srtp == nil || len(m.group.Epoch) == 0 {
		return
	}
	ourDeviceJid := m.ourDeviceJidLocked()
	if ourDeviceJid == "" {
		return
	}
	sendKM, err := media.DerivePerJidSrtpKey(m.group.Epoch, ourDeviceJid)
	if err != nil {
		m.log.Warn("group send key derivation failed",
			"call_id", call.CallID, "device", ourDeviceJid, "err", err)
		return
	}
	if m.groupSendKeySet {
		return
	}
	m.srtp.SetSendKey(sendKM)
	m.groupSendKeySet = true
	m.log.Info("group send key installed", "call_id", call.CallID, "device", ourDeviceJid)
}

// applyGroupAllocateLocked tells the relay the group shape and resends the
// allocate when the participant set changed. The resend is driven by the PID set
// itself, not by a relay transaction id: a participant that joins without the
// relay bumping that id would otherwise never have their media subscribed.
// Caller holds m.mu.
func (m *CallManager) applyGroupAllocateLocked() {
	call := m.currentCall
	if call == nil || m.relay == nil || m.group == nil || m.group.Roster == nil {
		return
	}
	ourDeviceJid := m.ourDeviceJidLocked()
	if ourDeviceJid == "" {
		m.log.Debug("group allocate deferred: own device JID not known yet",
			"call_id", call.CallID)
		return
	}
	streams := transport.DeriveRelayStreamSSRCs(call.CallID, ourDeviceJid)
	appData := media.GenerateSecureSsrc(call.CallID, ourDeviceJid, core.SsrcCounterAppData)
	streams, err := transport.PrepareRelayStreamSSRCs(streams, appData, rand.Reader)
	if err != nil {
		m.log.Warn("relay stream SSRC preparation failed", "call_id", call.CallID, "err", err)
		return
	}
	changed := m.relay.SetGroupAllocate(transport.GroupAllocateConfig{
		Streams:     streams,
		AppDataSSRC: appData,
		PIDs:        m.remotePIDsLocked(),
		HBHFEC: [2]uint32{
			media.GenerateSecureSsrc(call.CallID, ourDeviceJid, transport.HBHFECTXSlotWord),
			media.GenerateSecureSsrc(call.CallID, ourDeviceJid, transport.HBHFECRXSlotWord),
		},
	})
	if !changed {
		return
	}
	m.log.Info("group allocate updated",
		"call_id", call.CallID, "participants", len(m.remotePIDsLocked()))
	go m.relay.ResendSubscriptions()
}

// remotePIDsLocked lists the participant ids of every connected remote device.
// Caller holds m.mu.
func (m *CallManager) remotePIDsLocked() []uint32 {
	if m.group == nil || m.group.Roster == nil {
		return nil
	}
	ourBase := wanode.CleanJID(m.ownCredJid())
	var pids []uint32
	for _, participant := range m.group.Roster.Participants {
		for _, device := range participant.Devices {
			if !device.HasPID || device.JID.IsEmpty() {
				continue
			}
			if wanode.CleanJID(device.JID.String()) == ourBase {
				continue
			}
			pids = append(pids, device.PID)
		}
	}
	return pids
}

// ourDeviceJidLocked resolves this device's JID the same way the 1:1 SRTP setup
// does. Caller holds m.mu.
func (m *CallManager) ourDeviceJidLocked() string {
	call := m.currentCall
	if call == nil {
		return ""
	}
	var participants []string
	if call.RelayData != nil {
		participants = call.RelayData.ParticipantJids
	}
	ourBase := wanode.CleanJID(m.ownCredJid())
	return ensureDeviceJid(findOurDevice(participants, ourBase, m.ownCredJid()))
}

// HandleGroupOffer accepts an invite into an active group call. Unlike a 1:1
// offer it carries no call key: the shared epoch arrives afterwards over
// enc_rekey, and until it does the call has a roster but no media.
func (m *CallManager) HandleGroupOffer(
	ctx context.Context,
	node *waBinary.Node,
	peerJid types.JID,
	roster *signaling.GroupCallUpdate,
) {
	info := signaling.ExtractNodeInfo(node)
	if info == nil || roster == nil {
		return
	}
	creator := roster.CallCreator
	if creator.IsEmpty() {
		creator = peerJid
	}

	m.mu.Lock()
	call := NewIncomingCall(roster.CallID, peerJid.String(), creator.String(), "", core.CallMediaTypeAudio)
	m.currentCall = call
	m.group = &GroupState{Roster: roster, TransactionID: roster.TransactionID}
	m.mu.Unlock()

	m.log.Info("group call offer accepted",
		"call_id", roster.CallID,
		"transaction_id", roster.TransactionID,
		"participants", len(roster.Participants),
		"devices", countRosterDevices(roster))

	if m.OnIncoming != nil {
		m.OnIncoming(call)
	}
	m.emitState()

	requestID := signaling.GenerateCallStanzaID()
	preaccept, err := signaling.BuildActiveGroupPreaccept(roster.CallID, creator, requestID)
	if err != nil {
		m.log.Warn("group preaccept not built", "call_id", roster.CallID, "err", err)
		return
	}
	if err := m.sock.SendNode(ctx, preaccept); err != nil {
		m.log.Warn("group preaccept failed to send", "call_id", roster.CallID, "err", err)
	}
}

// selfInRosterLocked reports whether the server lists this device in the roster.
// A call where we never appear is a call the other participants cannot see us in.
// Caller holds m.mu.
func (m *CallManager) selfInRosterLocked(update *signaling.GroupCallUpdate) bool {
	ourBase := wanode.CleanJID(m.ownCredJid())
	for _, participant := range update.Participants {
		for _, device := range participant.Devices {
			if wanode.CleanJID(device.JID.String()) == ourBase {
				return true
			}
		}
	}
	return false
}

// describeRosterLocked renders the roster compactly for the log: who is on the
// call, in what state, and with which participant ids. Caller holds m.mu.
func describeRosterLocked(update *signaling.GroupCallUpdate) string {
	var b strings.Builder
	for i, participant := range update.Participants {
		if i > 0 {
			b.WriteString(" | ")
		}
		fmt.Fprintf(&b, "%s state=%s", participant.JID.User, participant.State)
		for _, device := range participant.Devices {
			fmt.Fprintf(&b, " dev=%s", device.JID.String())
			if device.HasPID {
				fmt.Fprintf(&b, ":pid%d", device.PID)
			}
		}
	}
	return b.String()
}
