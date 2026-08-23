package call

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/engine"
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
// groupEpochBytes is the shared key length the media plane requires.
const groupEpochBytes = 32

type GroupState struct {
	Roster        *signaling.GroupCallUpdate
	Epoch         []byte
	TransactionID uint32
	// epochTransactionID is the roster transaction the current epoch answers,
	// so a resent roster does not produce a second, diverging key.
	epochTransactionID uint32
	// RelayTokens and RelayKey are the credentials the roster reissues for this
	// call's relays, keyed by relay name.
	RelayTokens map[string][]byte
	RelayKey    []byte
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
		m.applyGroupUpdate(ctx, &envelope.Action)
	case "enc_rekey":
		m.applyGroupEpoch(ctx, envelope)
	}
}

func (m *CallManager) applyGroupUpdate(ctx context.Context, action *waBinary.Node) {
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
	m.connectGroupRelayLocked(update)
	m.distributeGroupEpochLocked(ctx, update)
	m.ensureGroupSrtpLocked()
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
		// Whether the roster carries the relay decides if media has anywhere to go.
		"has_relay", update.Relay != nil,
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
		Tokens:      m.group.RelayTokens,
		Key:         m.group.RelayKey,
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
	ourBase := wanode.CleanJID(m.ownCredJid())
	// Numa chamada de grupo o roster e quem diz qual device somos. O relay 1:1 nao
	// nos lista quando entramos por convite, e cair no JID base nos daria o device
	// 0: cada SSRC e cada chave sairia de uma identidade que nao esta na chamada.
	if m.group != nil && m.group.Roster != nil {
		for _, participant := range m.group.Roster.Participants {
			for _, device := range participant.Devices {
				raw := device.JID.String()
				if wanode.CleanJID(raw) == ourBase && strings.Contains(raw, ":") {
					return ensureDeviceJid(raw)
				}
			}
		}
	}
	var participants []string
	if call.RelayData != nil {
		participants = call.RelayData.ParticipantJids
	}
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

// distributeGroupEpochLocked answers a roster that asks for a rekey. The shared
// key is not handed to us: whoever joins generates it, encrypts it per device and
// hands it out. Ignoring the request gets this device demoted from connected back
// to invited and then dropped, which reads as "the call never let me in".
//
// Caller holds m.mu.
func (m *CallManager) distributeGroupEpochLocked(ctx context.Context, update *signaling.GroupCallUpdate) {
	if !update.RekeyRequested {
		return
	}
	// The roster is resent; distributing twice for one transaction would leave
	// participants holding different keys.
	if m.group != nil && m.group.epochTransactionID == update.TransactionID {
		return
	}
	recipients := m.rekeyRecipientsLocked(update)
	if len(recipients) == 0 {
		m.log.Warn("group rekey requested but no remote connected device has a PID",
			"call_id", update.CallID, "transaction_id", update.TransactionID)
		return
	}
	epoch := make([]byte, groupEpochBytes)
	if _, err := rand.Read(epoch); err != nil {
		m.log.Error("group epoch generation failed", "call_id", update.CallID, "err", err)
		return
	}
	nodes, _, err := m.sock.CreateParticipantNodes(ctx, recipients, epoch, waBinary.Attrs{"count": "0"})
	if err != nil {
		clear(epoch)
		m.log.Warn("group epoch encryption failed",
			"call_id", update.CallID, "transaction_id", update.TransactionID, "err", err)
		return
	}
	sent := 0
	for _, node := range nodes {
		to, ok := node.Attrs["jid"].(types.JID)
		if !ok {
			continue
		}
		enc, ok := node.GetOptionalChildByTag("enc")
		if !ok {
			continue
		}
		ciphertext, ok := enc.Content.([]byte)
		if !ok || len(ciphertext) == 0 {
			continue
		}
		rekey, buildErr := signaling.BuildGroupEncRekey(signaling.GroupEncRekeyParams{
			CallID: update.CallID, To: to, CallCreator: update.CallCreator,
			TransactionID: update.TransactionID, RequestID: signaling.GenerateCallStanzaID(),
			DeviceKey: signaling.GroupOfferDeviceKey{
				DeviceJID:  to,
				EncType:    enc.AttrGetter().String("type"),
				Ciphertext: ciphertext,
			},
		})
		if buildErr != nil {
			m.log.Warn("group rekey not built", "call_id", update.CallID, "to", to, "err", buildErr)
			continue
		}
		// One failed recipient is one silent participant, not a dead call.
		if sendErr := m.sock.SendNode(ctx, rekey); sendErr != nil {
			m.log.Warn("group rekey send failed", "call_id", update.CallID, "to", to, "err", sendErr)
			continue
		}
		sent++
	}
	if m.group == nil {
		m.group = &GroupState{}
	}
	m.group.Epoch = append([]byte(nil), epoch...)
	m.group.epochTransactionID = update.TransactionID
	clear(epoch)
	m.ensureGroupSrtpLocked()
	m.log.Info("group epoch distributed",
		"call_id", update.CallID, "transaction_id", update.TransactionID,
		"recipients", len(recipients), "sent", sent)
	m.syncGroupRecvKeysLocked()
}

// rekeyRecipientsLocked lists the remote devices that should hold the epoch: the
// ones the server marked connected and gave a participant id. A device without a
// PID has no media routed to it, so a key would be wasted. Caller holds m.mu.
func (m *CallManager) rekeyRecipientsLocked(update *signaling.GroupCallUpdate) []types.JID {
	ourBase := wanode.CleanJID(m.ownCredJid())
	seen := map[string]bool{}
	var out []types.JID
	for _, participant := range update.Participants {
		if participant.State != "connected" {
			continue
		}
		for _, device := range participant.Devices {
			if !device.HasPID || device.JID.IsEmpty() {
				continue
			}
			raw := device.JID.String()
			if wanode.CleanJID(raw) == ourBase || seen[raw] {
				continue
			}
			seen[raw] = true
			out = append(out, device.JID)
		}
	}
	return out
}

// ensureGroupSrtpLocked builds the SRTP session a group call needs. The 1:1 setup
// keys off the per-call key, which a group call does not have: its media keys off
// the shared epoch. Without this every later step stays blocked, because they all
// require an SRTP session to exist. Caller holds m.mu.
func (m *CallManager) ensureGroupSrtpLocked() {
	if m.srtp != nil || m.group == nil || len(m.group.Epoch) == 0 {
		return
	}
	call := m.currentCall
	if call == nil {
		return
	}
	ourDeviceJid := m.ourDeviceJidLocked()
	if ourDeviceJid == "" {
		return
	}
	sendKM, err := media.DerivePerJidSrtpKey(m.group.Epoch, ourDeviceJid)
	if err != nil {
		m.log.Error("group SRTP key derivation failed", "call_id", call.CallID, "err", err)
		return
	}
	// The receive side has no single key in a group call: every participant gets
	// its own through SetRecvKeyForSSRC. This one is only the fallback for a
	// sender we were never told about, and it must not authenticate anything.
	m.srtp = engine.NewSrtpManager(sendKM, core.SrtpKeyingMaterial{}, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	m.srtp.SetObserver(m.observer)
	m.groupSendKeySet = true
	m.log.Info("group SRTP session created", "call_id", call.CallID, "device", ourDeviceJid)
	m.ensureExtensionsAttachedLocked(ourDeviceJid, "")
}

// connectGroupRelayLocked turns the <relay> block the roster carries into
// transport endpoints. It is where a group call's media travels; without it the
// call has a roster, keys, and nowhere to send audio. Caller holds m.mu.
func (m *CallManager) connectGroupRelayLocked(update *signaling.GroupCallUpdate) {
	if update.Relay == nil || len(update.Relay.Endpoints) == 0 || m.relay == nil {
		return
	}
	call := m.currentCall
	if call == nil {
		return
	}
	tokens := make(map[string][]byte)
	for _, endpoint := range update.Relay.Endpoints {
		if endpoint.RelayName == "" {
			continue
		}
		if token := groupTokenAt(update.Relay.Tokens, endpoint.TokenID); len(token) > 0 {
			tokens[endpoint.RelayName] = token
		}
	}
	m.group.RelayTokens = tokens
	m.group.RelayKey = update.Relay.Key

	// A call upgraded in place from 1:1 already has the connection that carries
	// media, and redialing would abandon it while the new connections sit in ICE
	// checking until they time out.
	if m.relay.HasConnection() {
		m.log.Info("group relay credentials applied to the open connection",
			"call_id", call.CallID, "relays", len(tokens),
			"self_pid", update.Relay.SelfPID)
		return
	}
	relayData := groupRelayToCore(update.Relay)
	if len(relayData.Endpoints) == 0 {
		return
	}
	if call.RelayData != nil && sameRelayEndpoints(call.RelayData.Endpoints, relayData.Endpoints) {
		return
	}
	call.RelayData = relayData
	m.log.Info("group relay endpoints applied",
		"call_id", call.CallID, "endpoints", len(relayData.Endpoints),
		"self_pid", update.Relay.SelfPID)
	endpoints := relayData.Endpoints
	go m.connectRelays(endpoints)
}

func groupRelayToCore(relay *signaling.GroupCallRelay) *core.RelayData {
	out := &core.RelayData{
		UUID:   relay.UUID,
		HbhKey: relay.HBHKey,
	}
	if relay.HasSelfPID {
		pid := int(relay.SelfPID)
		out.SelfPid = &pid
	}
	for _, endpoint := range relay.Endpoints {
		if endpoint.IPv4 == "" || endpoint.Port == 0 {
			continue
		}
		converted := core.RelayEndpoint{
			IP: endpoint.IPv4, Port: int(endpoint.Port),
			RelayName: endpoint.RelayName, RelayID: int(endpoint.RelayID),
			Key: string(relay.Key), IsFNA: endpoint.IsFNA,
			AddressBytes: endpoint.Address,
		}
		// The ICE handshake reads the token as text, so it travels base64-encoded
		// exactly as the 1:1 relay ACK encodes it. The raw bytes stay for the STUN
		// allocate: feeding them to ICE leaves the connection in checking until it
		// times out.
		if token := groupTokenAt(relay.Tokens, endpoint.TokenID); len(token) > 0 {
			converted.RawToken = token
			converted.Token = base64.StdEncoding.EncodeToString(token)
		}
		if token := groupTokenAt(relay.AuthTokens, endpoint.AuthTokenID); len(token) > 0 {
			converted.RawAuthToken = token
			converted.AuthToken = base64.StdEncoding.EncodeToString(token)
		}
		if endpoint.RTT != 0 {
			rtt := int(endpoint.RTT)
			converted.C2RRtt = &rtt
		}
		out.Endpoints = append(out.Endpoints, converted)
	}
	return out
}

func sameRelayEndpoints(a, b []core.RelayEndpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].IP != b[i].IP || a[i].Port != b[i].Port {
			return false
		}
	}
	return true
}

// groupTokenAt reads a token the endpoint addresses by index.
func groupTokenAt(tokens [][]byte, id uint32) []byte {
	if int(id) >= len(tokens) {
		return nil
	}
	return tokens[id]
}
