package call

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/engine"
	"wacalls/internal/voip/media"
	"wacalls/internal/voip/signaling"
	"wacalls/internal/voip/transport"
	"wacalls/internal/voip/wanode"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

const signalingSendTimeout = 10 * time.Second

type CallManager struct {
	sock     signaling.Socket
	log      *slog.Logger
	observer core.CallObserver

	mu          sync.Mutex
	currentCall *CallInfo

	rtpSession *media.RtpSession
	srtp       *engine.SrtpManager
	relay      RelayTransport

	selfSsrc      uint32
	peerSsrcs     []uint32
	actualPeerSet bool

	firstPacketSent       bool
	initialTransportSent  bool
	outgoingPreacceptSent bool
	observerEnded         bool
	acceptedByJid         string
	calleeDevices         []types.JID
	debeEnabled           bool

	timeouts      Timeouts
	watchdogTick  time.Duration
	watchdogStop  chan struct{}
	lastMediaRecv atomic.Int64
	lastRedialAt  time.Time
	srtpDrops     srtpDropTally

	sendSrtcp      *media.SrtcpContext
	recvSrtcp      *media.SrtcpContext
	srtcpDrops     srtpDropTally
	recvStats      *media.RTCPReceiverStats
	rtcpTxStop     chan struct{}
	rtcpCName      string
	srtcpTxIndex   uint32
	rtpPacketsSent uint32
	rtpOctetsSent  uint32
	lastRtpTs      uint32
	rtcp208Tick    time.Duration
	rtcpSRTick     time.Duration
	rtcp209Tick    time.Duration

	lastReactionAt time.Time

	// group stays nil on a 1:1 call.
	group *GroupState
	// groupSendKeySet guards against re-keying the send side on every roster
	// update, which would reset the outbound SRTP contexts mid-call.
	groupSendKeySet bool

	extensions   []engine.Extension
	extMu        sync.Mutex
	rtpHandlers  map[uint8]func(*media.RtpPacket)
	declaredSelf map[uint32]bool
	seenInbound  map[uint64]bool
	extAttached  bool

	OnStateChange func(*CallInfo)
	OnIncoming    func(*CallInfo)
	OnEnded       func(*CallInfo)
	OnPeerAudio   func([]float32)
	OnQuality     func(callID string, q core.CallQuality)
	OnMark        func(callID string, mark string, elapsedMs int64)
	OnRelay       func(callID, relayName string, rttMs int, hasRtt bool)
	OnPeerMute    func(callID string, muted bool)
	OnReaction    func(callID, emoji string)
}

// reactionMinInterval throttles outbound reactions: each one costs ten packets on
// the relay uplink, which competes with audio if a client repeats the click.
const reactionMinInterval = 500 * time.Millisecond

func NewCallManager(sock signaling.Socket, log *slog.Logger, exts ...engine.Extension) *CallManager {
	if log == nil {
		log = slog.Default()
	}
	m := &CallManager{
		sock:         sock,
		log:          log,
		observer:     core.NopObserver{},
		debeEnabled:  true,
		timeouts:     DefaultTimeouts,
		watchdogTick: defaultWatchdogTick,
		rtcp208Tick:  rtcp208Interval,
		rtcpSRTick:   rtcpSRInterval,
		rtcp209Tick:  rtcp209Interval,
		extensions:   exts,
		rtpHandlers:  map[uint8]func(*media.RtpPacket){},
		declaredSelf: map[uint32]bool{},
	}
	relay := transport.NewSctpRelayManager(log)
	relay.SetOnConnected(func(ip string, port int) { m.onRelayConnected(ip, port) })
	relay.SetOnReceive(func(data []byte) { m.onRelayData(data) })
	relay.SetOnUsableChange(func(usable int) { m.onRelayUsableChange(usable) })
	m.relay = relay
	return m
}

func (m *CallManager) CurrentCall() *CallInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentCall
}

func (m *CallManager) emitState() {
	if m.OnStateChange != nil && m.currentCall != nil {
		m.OnStateChange(m.currentCall)
	}
}

func (m *CallManager) StartCall(ctx context.Context, callID string, peerJid types.JID) error {
	m.mu.Lock()
	if m.currentCall != nil && !m.currentCall.IsEnded() {
		m.mu.Unlock()
		return &CallError{"a call is already in progress"}
	}

	mediaType := core.CallMediaTypeAudio
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
	m.replaceRtpSession(media.NewWhatsAppOpusSession(m.selfSsrc))
	m.peerSsrcs = []uint32{media.GenerateSecureSsrc(callID, resolved.String(), 0)}
	m.mu.Unlock()

	offer, calleeDevices, err := signaling.BuildOfferStanza(ctx, m.sock, callID, callKey, resolved)
	if err != nil {
		return err
	}
	ackNode, err := m.sock.Query(ctx, offer)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.calleeDevices = calleeDevices
	_ = m.currentCall.ApplyTransition(Transition{Type: TransitionOfferSent})
	m.emitState()
	m.mu.Unlock()

	if ackNode != nil {
		go m.HandleCallAck(context.Background(), ackNode)
	}

	m.startWatchdog()
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
	relayData := call.RelayData
	isGroup := m.group != nil
	m.mu.Unlock()

	// A group call has no per-call key: the media keys off the shared epoch, and
	// its accept carries no <enc>. Falling through to the 1:1 path here sends
	// nothing at all, because that path is guarded on the key being present, and
	// the caller's phone keeps ringing until it times out.
	if isGroup {
		accept, err := signaling.BuildActiveGroupAccept(callID, creator, signaling.GenerateCallStanzaID())
		if err != nil {
			m.log.Error("build group accept failed", "call_id", callID, "err", err)
			return err
		}
		if err := m.sock.SendNode(ctx, accept); err != nil {
			m.log.Error("group accept send error", "call_id", callID, "err", err)
			return err
		}
		m.log.Info("group call accepted", "call_id", callID)
		return nil
	}

	if key != nil {
		acceptNode, err := signaling.BuildAcceptStanza(ctx, m.sock, callID, key, peer, creator)
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
			m.replaceRtpSession(media.NewWhatsAppOpusSession(newSelf))
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
	if err := call.ApplyTransition(Transition{Type: TransitionLocalRejected, Reason: reason}); err != nil {
		m.mu.Unlock()
		return err
	}
	node := signaling.BuildRejectStanza(wanode.MustJID(call.PeerJid), call.CallID, wanode.MustJID(call.CallCreator))
	m.emitState()
	m.mu.Unlock()

	m.sendSignaling(ctx, node)
	m.cleanupMedia()
	return nil
}

func (m *CallManager) sendSignaling(ctx context.Context, node waBinary.Node) {
	go func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), signalingSendTimeout)
		defer cancel()
		_, _ = m.sock.Query(sctx, node)
	}()
}

func (m *CallManager) EndCall(ctx context.Context, reason core.EndCallReason) error {
	m.mu.Lock()
	call := m.currentCall
	if call == nil || call.IsEnded() {
		m.mu.Unlock()
		return nil
	}
	_ = call.ApplyTransition(Transition{Type: TransitionTerminated, Reason: reason})
	// Route the terminate to the device that actually answered, like the rest of the in-call
	// signaling. Sending it to the base JID lets the server deliver it to the peer's primary
	// device, so a call answered on a companion (e.g. WhatsApp Web) never sees the terminate and
	// hangs in "reconnecting" until it times out. acceptedByJid is empty for inbound calls, where
	// call.PeerJid already carries the caller's device.
	termDest := call.PeerJid
	if m.acceptedByJid != "" {
		termDest = m.acceptedByJid
	}
	node := signaling.BuildTerminateStanza(wanode.MustJID(termDest), call.CallID, wanode.MustJID(call.CallCreator))
	ended := call
	m.emitState()
	m.mu.Unlock()

	m.sendSignaling(ctx, node)
	if m.OnEnded != nil {
		m.OnEnded(ended)
	}
	m.cleanupMedia()
	return nil
}

func (m *CallManager) SetMute(ctx context.Context, muted bool) error {
	m.mu.Lock()
	call := m.currentCall
	if call == nil || call.IsEnded() {
		m.mu.Unlock()
		return &CallError{"no active call"}
	}
	if err := call.ApplyTransition(Transition{Type: TransitionAudioMuteChanged, Muted: muted}); err != nil {
		m.mu.Unlock()
		return err
	}
	// In-call signaling targets the device that answered, like the terminate path.
	dest := call.PeerJid
	if m.acceptedByJid != "" {
		dest = m.acceptedByJid
	}
	state := 0
	if muted {
		state = 1
	}
	node := signaling.BuildMuteV2Stanza(wanode.MustJID(dest), call.CallID, wanode.MustJID(call.CallCreator), state)
	m.emitState()
	m.mu.Unlock()

	m.sendSignaling(ctx, node)
	return nil
}

// SendReaction sends an emoji reaction over the app-data media stream. Unlike
// SetMute this rides media, not signaling, so it applies no state transition.
func (m *CallManager) SendReaction(emoji string) error {
	m.mu.Lock()
	call := m.currentCall
	if call == nil || call.IsEnded() {
		m.mu.Unlock()
		return &CallError{"no active call"}
	}
	if !m.lastReactionAt.IsZero() && time.Since(m.lastReactionAt) < reactionMinInterval {
		m.mu.Unlock()
		return &CallError{"reaction rate limited"}
	}
	m.lastReactionAt = time.Now()
	m.mu.Unlock()

	sink, ok := engine.Capability[core.ReactionSink](m.extensions)
	if !ok {
		return &CallError{"reactions unavailable on this call"}
	}
	return sink.SendReaction(emoji)
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
