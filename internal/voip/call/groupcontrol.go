package call

import (
	"context"

	"wacalls/internal/voip/signaling"

	waBinary "go.mau.fi/whatsmeow/binary"
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
	m.log.Info("group roster applied",
		"call_id", update.CallID,
		"transaction_id", update.TransactionID,
		"participants", len(update.Participants),
		"devices", countRosterDevices(update))
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
