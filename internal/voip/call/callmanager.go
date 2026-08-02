package call

import (
	"context"
	"log/slog"
	"sync"
	"time"
	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
	"wacalls/internal/voip/signaling"
	"wacalls/internal/voip/transport"
	"wacalls/internal/voip/wanode"

	"go.mau.fi/whatsmeow/types"
)

type CallManager struct {
	sock core.VoipSocket
	log  *slog.Logger

	mu          sync.Mutex
	currentCall *CallInfo

	rtpSession  *media.RtpSession
	srtpSession *media.SrtpSession
	codec       media.Codec
	relay       RelayTransport

	selfSsrc      uint32
	peerSsrcs     []uint32
	actualPeerSet bool

	emitMu       sync.Mutex
	emitPending  []*CallInfo
	emitDraining bool

	firstPacketSent       bool
	initialTransportSent  bool
	outgoingPreacceptSent bool
	acceptedByJid         string
	debeEnabled           bool

	encodeBuf    []float32
	encodeBufPos int

	lastCaptureAt time.Time
	keepaliveStop chan struct{}

	OnStateChange func(*CallInfo)
	OnIncoming    func(*CallInfo)
	OnEnded       func(*CallInfo)
	OnPeerAudio   func([]float32)
}

func NewCallManager(sock core.VoipSocket, log *slog.Logger) *CallManager {
	if log == nil {
		log = slog.Default()
	}
	m := &CallManager{
		sock:        sock,
		log:         log,
		debeEnabled: true,
	}
	relay := transport.NewSctpRelayManager(log)
	relay.SetOnConnected(func(ip string, port int) { m.onRelayConnected() })
	relay.SetOnReceive(func(data []byte) { m.onRelayData(data) })
	m.relay = relay
	return m
}

func (m *CallManager) CurrentCall() *CallInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentCall
}

// emitState queues the current call state for delivery. Every caller holds
// m.mu, so the host callback must NOT run here: consumers do real work on it
// (the reference server closes the WebRTC peer connection on the ended branch),
// and running that under the manager lock serialises the whole call — or
// deadlocks outright if the consumer calls back into the manager.
func (m *CallManager) emitState() {
	if m.currentCall == nil {
		return
	}
	m.queueEmit(emitSnapshot(m.currentCall))
}

func (m *CallManager) queueEmit(snap *CallInfo) {
	m.emitMu.Lock()
	m.emitPending = append(m.emitPending, snap)
	if m.emitDraining {
		m.emitMu.Unlock()
		return
	}
	m.emitDraining = true
	m.emitMu.Unlock()
	go m.drainEmits()
}

// drainEmits delivers every queued snapshot to OnStateChange, in order, holding
// no manager lock. Exactly one instance runs at a time — a goroutine per event
// would be simpler but would reorder them, and the order is semantic: a
// consumer must never see "connected" after "ended".
func (m *CallManager) drainEmits() {
	for {
		m.emitMu.Lock()
		if len(m.emitPending) == 0 {
			m.emitDraining = false
			m.emitMu.Unlock()
			return
		}
		snap := m.emitPending[0]
		m.emitPending[0] = nil
		m.emitPending = m.emitPending[1:]
		m.emitMu.Unlock()

		if cb := m.OnStateChange; cb != nil {
			cb(snap)
		}
	}
}

// emitSnapshot copies the call as the host will see it. The host now runs after
// the lock is released, so handing it the live *CallInfo would let the manager
// mutate the struct under the consumer's feet (and trip -race). The time
// pointers in StateData are copied for the same reason.
func emitSnapshot(c *CallInfo) *CallInfo {
	snap := *c
	snap.StateData.ConnectedAt = copyTimePtr(c.StateData.ConnectedAt)
	snap.StateData.AcceptedAt = copyTimePtr(c.StateData.AcceptedAt)
	snap.StateData.EndedAt = copyTimePtr(c.StateData.EndedAt)
	return &snap
}

func copyTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	tt := *t
	return &tt
}

func (m *CallManager) StartCall(ctx context.Context, callID string, peerJid types.JID, isVideo bool) error {
	m.mu.Lock()
	if m.currentCall != nil && !m.currentCall.IsEnded() {
		m.mu.Unlock()
		return &CallError{"a call is already in progress"}
	}

	mediaType := core.CallMediaTypeAudio
	if isVideo {
		mediaType = core.CallMediaTypeVideo
	}
	creator := m.sock.OwnLID()
	if creator.IsEmpty() {
		creator = m.sock.OwnPN()
	}
	resolved := m.sock.ResolveLIDForPN(ctx, peerJid)

	call := NewOutgoingCall(callID, resolved.String(), creator.String(), mediaType)
	callKey := media.GenerateCallKey()
	call.EncryptionKey = callKey
	m.currentCall = call
	m.initialTransportSent = false
	m.outgoingPreacceptSent = false

	selfJid := creator.String()
	m.selfSsrc = media.GenerateSecureSsrc(callID, selfJid, 0)
	m.rtpSession = media.NewWhatsAppOpusSession(m.selfSsrc)
	m.peerSsrcs = []uint32{media.GenerateSecureSsrc(callID, resolved.String(), 0)}
	m.initCodec()
	m.mu.Unlock()

	offer, err := signaling.BuildOfferStanza(ctx, m.sock, callID, callKey, resolved, isVideo)
	if err != nil {
		return err
	}
	ackNode, err := m.sock.Query(ctx, offer)
	if err != nil {
		return err
	}

	m.mu.Lock()
	_ = m.currentCall.ApplyTransition(Transition{Type: TransitionOfferSent})
	m.emitState()
	m.mu.Unlock()

	if ackNode != nil {
		go m.HandleCallAck(context.Background(), ackNode)
	}

	m.log.Info("call offer sent", "call_id", callID, "peer", resolved.String())
	return nil
}

func (m *CallManager) AcceptCall(ctx context.Context, callID string) error {
	m.mu.Lock()
	call := m.currentCall
	if call == nil || call.CallID != callID {
		m.mu.Unlock()
		return &CallError{"no incoming call with id " + callID}
	}
	if !call.CanAccept() {
		m.mu.Unlock()
		return &CallError{"call cannot be accepted in state " + string(call.StateData.State)}
	}
	_ = call.ApplyTransition(Transition{Type: TransitionLocalAccepted})
	m.emitState()
	key := call.EncryptionKey
	peer := wanode.MustJID(call.PeerJid)
	creator := wanode.MustJID(call.CallCreator)
	isVideo := call.MediaType == core.CallMediaTypeVideo
	relayData := call.RelayData
	m.mu.Unlock()

	if key != nil {
		acceptNode, err := signaling.BuildAcceptStanza(ctx, m.sock, callID, key, peer, creator, isVideo)
		if err != nil {
			m.log.Error("build accept failed", "err", err)
		} else if err := m.sock.SendNode(ctx, acceptNode); err != nil {
			m.log.Error("accept send error", "err", err)
		}
	}

	if relayData != nil {
		m.setupIncomingMedia(call, relayData)
		m.connectRelays(relayData.Endpoints)
	} else {
		m.log.Warn("call accepted but no relay endpoints yet; media path waits for a transport message", "call_id", callID)
	}
	m.log.Info("call accepted", "call_id", callID)
	return nil
}

func (m *CallManager) setupIncomingMedia(call *CallInfo, relayData *core.RelayData) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(relayData.ParticipantJids) > 0 {
		ourBase := wanode.CleanJID(m.ownCredJid())
		ourDeviceJid := ensureDeviceJid(findOurDevice(relayData.ParticipantJids, ourBase, m.ownCredJid()))
		if newSelf := media.GenerateSecureSsrc(call.CallID, ourDeviceJid, 0); newSelf != m.selfSsrc {
			m.selfSsrc = newSelf
			m.rtpSession = media.NewWhatsAppOpusSession(newSelf)
		}
		if peer := firstPeerDevice(relayData.ParticipantJids, ourBase); peer != "" {
			m.peerSsrcs = []uint32{media.GenerateSecureSsrc(call.CallID, ensureDeviceJid(peer), 0)}
			m.actualPeerSet = true
		}
	}
	m.relay.SetSubscriptionSsrc(firstSsrc(m.peerSsrcs))
	m.initSrtpKeysLocked()
}

func (m *CallManager) RejectCall(ctx context.Context, callID string, reason core.EndCallReason) error {
	m.mu.Lock()
	call := m.currentCall
	if call == nil || call.CallID != callID {
		m.mu.Unlock()
		return &CallError{"no call with id " + callID}
	}
	_ = call.ApplyTransition(Transition{Type: TransitionLocalRejected, Reason: reason})
	node := signaling.BuildRejectStanza(wanode.MustJID(call.PeerJid), call.CallID, wanode.MustJID(call.CallCreator))
	m.emitState()
	m.mu.Unlock()

	go func() { _, _ = m.sock.Query(ctx, node) }()
	m.cleanupMedia()
	return nil
}

func (m *CallManager) EndCall(ctx context.Context, reason core.EndCallReason) error {
	m.mu.Lock()
	call := m.currentCall
	if call == nil || call.IsEnded() {
		m.mu.Unlock()
		return nil
	}
	_ = call.ApplyTransition(Transition{Type: TransitionTerminated, Reason: reason})
	node := signaling.BuildTerminateStanza(wanode.MustJID(call.PeerJid), call.CallID, wanode.MustJID(call.CallCreator))
	ended := call
	m.emitState()
	m.mu.Unlock()

	go func() { _, _ = m.sock.Query(ctx, node) }()
	if m.OnEnded != nil {
		m.OnEnded(ended)
	}
	m.cleanupMedia()
	return nil
}

func (m *CallManager) ownCredJid() string {
	lid := m.sock.OwnLID()
	if !lid.IsEmpty() {
		return lid.String()
	}
	return m.sock.OwnPN().String()
}

type CallError struct{ Msg string }

func (e *CallError) Error() string { return e.Msg }
