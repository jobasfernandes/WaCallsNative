package call

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/engine"
	"wacalls/internal/voip/media"
	"wacalls/internal/voip/transport"
)

const rtpSessionBytes = 8 * 1024

func (m *CallManager) replaceRtpSession(s *media.RtpSession) {
	if m.rtpSession != nil {
		m.observer.ReleaseMem(rtpSessionBytes)
	}
	m.rtpSession = s
	if s != nil {
		m.observer.AddMem(rtpSessionBytes)
	}
}

func (m *CallManager) FeedCapturedPCM(data []float32) {
	if a, ok := engine.Capability[core.AudioSink](m.extensions); ok {
		a.FeedPCM(data)
	}
}

func (m *CallManager) registerRTPHandler(pt uint8, handler func(*media.RtpPacket)) {
	m.extMu.Lock()
	m.rtpHandlers[pt] = handler
	m.extMu.Unlock()
}

func (m *CallManager) declareSelfSSRC(ssrc uint32) {
	m.extMu.Lock()
	m.declaredSelf[ssrc] = true
	m.extMu.Unlock()
}

func (m *CallManager) ensureExtensionsAttachedLocked(ourDeviceJid, peerDeviceJid string) {
	if m.extAttached {
		return
	}
	m.extAttached = true
	scope := &engine.CallScope{
		Log:             m.log,
		CallID:          m.currentCall.CallID,
		OwnDeviceJID:    ourDeviceJid,
		PeerDeviceJID:   peerDeviceJid,
		Relay:           m.relay,
		SendAudioFrame:  m.sendAudioFrame,
		SendRTP:         m.sendRTP,
		OnRTP:           m.registerRTPHandler,
		DeclareSelfSSRC: m.declareSelfSSRC,
		Observer:        m.observer,
	}
	for _, e := range m.extensions {
		if err := e.Attach(scope); err != nil {
			m.log.Error("extension attach failed", "ext", e.Name(), "err", err)
		}
	}
	if a, ok := engine.Capability[core.AudioSink](m.extensions); ok {
		a.OnPeerPCM(func(pcm []float32) {
			if m.OnPeerAudio != nil {
				m.OnPeerAudio(pcm)
			}
		})
	}
	if r, ok := engine.Capability[core.ReactionSink](m.extensions); ok {
		// callID sai do scope antes do closure: o callback roda depois, quando
		// m.mu pode estar tomado por outro caminho.
		callID := scope.CallID
		r.OnPeerReaction(func(emoji string) {
			if m.OnReaction != nil {
				m.OnReaction(callID, emoji)
			}
		})
	}
}

// resetReactionState drops the dedup high-water mark held by the app-data
// extension. Both media restarts need it: the peer's sender starts counting from
// one again, and a stale mark swallows every later reaction without a trace.
func (m *CallManager) resetReactionState() {
	if r, ok := engine.Capability[core.ReactionSink](m.extensions); ok {
		if ext, ok := r.(interface{ ResetPeerState() }); ok {
			ext.ResetPeerState()
		}
	}
}

func (m *CallManager) sendAudioFrame(encoded []byte, frameSamples int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rtpSession == nil || m.srtp == nil {
		return nil
	}
	marker := !m.firstPacketSent
	if marker {
		m.observer.Mark(core.MarkMediaFirstPacket)
	}
	pkt := m.rtpSession.CreatePacketWithDuration(encoded, frameSamples, marker)
	if m.debeEnabled {
		pkt.Header.Extension = true
		pkt.Header.ExtensionProfile = 0xbede
		pkt.Header.ExtensionData = nil
	}
	m.firstPacketSent = true
	m.rtpPacketsSent++
	m.rtpOctetsSent += uint32(len(pkt.Payload))
	m.lastRtpTs = pkt.Header.Timestamp
	protected, err := m.srtp.Protect(pkt)
	if err != nil {
		m.log.Debug("srtp protect error", "err", err)
		return err
	}
	m.relay.Broadcast(protected)
	return nil
}

// sendRTP protects and broadcasts a packet an extension built itself. Unlike
// sendAudioFrame it owns no sequence/timestamp state: a stream other than audio
// keeps its own, so nothing here touches the audio counters or the RTCP stats.
func (m *CallManager) sendRTP(pkt *media.RtpPacket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.srtp == nil || m.relay == nil {
		return &CallError{"media not established"}
	}
	protected, err := m.srtp.Protect(pkt)
	if err != nil {
		m.log.Debug("srtp protect error", "pt", pkt.Header.PayloadType, "err", err)
		return err
	}
	m.relay.Broadcast(protected)
	return nil
}

func (m *CallManager) notePeerMedia(ssrc uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ssrc == m.selfSsrc {
		return
	}
	m.notePeerMediaLocked()
}

func (m *CallManager) notePeerMediaLocked() {
	m.lastMediaRecv.Store(time.Now().UnixMilli())
	if m.currentCall != nil && m.currentCall.StateData.State == core.CallStateReconnecting {
		if err := m.currentCall.ApplyTransition(Transition{Type: TransitionMediaRestored}); err == nil {
			m.emitState()
			m.log.Info("media path restored", "call_id", m.currentCall.CallID)
		}
	}
}

func (m *CallManager) onRelayData(data []byte) {
	// In multi-participant mode the relay prepends a forwarding header to the
	// media it bridges. Left wrapped, the packet matches neither STUN nor RTP and
	// falls off the end of this function unlogged, which is why no participant is
	// ever heard.
	if payload, wrapped, valid := transport.UnwrapGroupForwardingPacket(data); wrapped {
		// A header without a payload is relay bookkeeping, not media.
		if valid && len(payload) == 0 {
			return
		}
		if !valid {
			// The subtype is what names the header length, so it is the only thing
			// that says whether this is a shape we do not know or a truncated one.
			m.noteUnroutable(
				fmt.Sprintf("malformed forwarding header subtype=%#02x", data[1]), len(data))
			return
		}
		data = payload
	}
	if transport.IsStunPacket(data) {
		// The relay answers the allocate here. Discarding it unread is why a
		// refused subscription looks exactly like a relay that stays silent.
		m.noteStunResponse(data)
		return
	}
	if transport.IsRtcpPacket(data) {
		m.noteInboundRTCP()
		senderSsrc, _ := media.ParseRTCPSenderSSRC(data)
		m.notePeerMedia(senderSsrc)

		m.mu.Lock()
		recvSrtcp := m.recvSrtcp
		recvStats := m.recvStats
		selfSsrc := m.selfSsrc
		obs := m.observer
		callID := ""
		if m.currentCall != nil {
			callID = m.currentCall.CallID
		}
		m.mu.Unlock()
		if recvSrtcp == nil || recvStats == nil {
			return
		}
		plain, err := recvSrtcp.Unprotect(data)
		if err != nil {
			// SRTCP decode failures are control-plane telemetry drops, kept apart from the
			// media-plane SrtpRecvDrop counter/tally so an RTCP fault cannot mask or conflate a
			// genuine audio-decode fault under the same reason label.
			reason := "other"
			var se *media.SrtpError
			if errors.As(err, &se) {
				reason = string(se.Type)
			}
			if m.srtcpDrops.add(reason) {
				m.log.Warn("srtcp recv packet dropped", "reason", reason, "err", err)
			}
			return
		}
		now := uint64(time.Now().UnixMilli())
		in := media.ParseRTCPCompound(plain)
		if in.HasSR {
			recvStats.NoteSenderReport(in.SRNtpMid, now)
		}
		for _, b := range in.Blocks {
			if b.SSRC == selfSsrc && b.LSR != 0 {
				recvStats.NotePeerReportBlock(b.LSR, b.DLSR, b.FractionLost, now)
			}
		}
		q := recvStats.QualitySnapshot(now)
		obs.NoteQuality(q)
		if m.OnQuality != nil && callID != "" {
			m.OnQuality(callID, q)
		}
		return
	}
	if !transport.IsRtpPacket(data) {
		m.noteUnroutable("neither stun nor rtp", len(data))
		return
	}
	if len(data) < 12 {
		m.noteUnroutable("shorter than an rtp header", len(data))
		return
	}
	pt := data[1] & 0x7f
	ssrc := media.RTPSsrc(data)

	m.mu.Lock()
	if ssrc == m.selfSsrc {
		m.mu.Unlock()
		return
	}
	m.notePeerMediaLocked()
	srtp := m.srtp
	obs := m.observer
	recvStats := m.recvStats
	m.mu.Unlock()

	m.extMu.Lock()
	skip := m.declaredSelf[ssrc]
	handler := m.rtpHandlers[pt]
	m.extMu.Unlock()
	// Every inbound stream is announced once. Silence here is itself the finding:
	// it separates "the relay forwards nothing" from "packets arrive and are
	// dropped", which no other log distinguishes.
	m.noteInboundStream(ssrc, pt, skip, srtp != nil, handler != nil)
	if skip || srtp == nil || handler == nil {
		return
	}

	pkt, err := srtp.Unprotect(data)
	if err != nil {
		reason := "other"
		var se *media.SrtpError
		if errors.As(err, &se) {
			reason = string(se.Type)
		}
		obs.SrtpRecvDrop(reason)
		m.dumpUnauthenticated(data)
		if m.srtpDrops.add(reason) {
			m.log.Warn("srtp recv packet dropped", "reason", reason, "err", err)
		} else {
			m.log.Debug("srtp recv packet dropped", "reason", reason, "err", err)
		}
		return
	}
	if len(pkt.Payload) == 0 {
		return
	}
	// Subscription bootstrap only after the packet authenticated: RTP-shaped bytes
	// with a spoofed SSRC must never redirect the peer subscription.
	if core.IsWhatsAppAudioPayload(pt) {
		m.mu.Lock()
		// A group call subscribes to every participant at once, and the roster is
		// what says who they are. Latching onto the first stream that authenticates
		// would drop every other participant's subscription.
		if !m.actualPeerSet && m.group == nil {
			m.actualPeerSet = true
			if !containsSsrc(m.peerSsrcs, ssrc) {
				m.peerSsrcs = []uint32{ssrc}
				m.relay.SetSubscriptionSsrc(ssrc)
				go m.relay.ResendSubscriptions()
			}
		}
		m.mu.Unlock()
	}
	// Only the audio stream feeds the quality metrics: it is the one continuous
	// stream, so jitter and loss mean something. Sporadic streams carry their own
	// sequence and timestamp space and would read as huge loss.
	if recvStats != nil && core.IsWhatsAppAudioPayload(pt) {
		recvStats.NoteRTP(pkt.Header.SequenceNumber, pkt.Header.Timestamp, uint64(time.Now().UnixMilli()))
	}
	handler(pkt)
}

type srtpDropTally struct {
	mu     sync.Mutex
	counts map[string]int64
}

func (t *srtpDropTally) add(reason string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counts == nil {
		t.counts = map[string]int64{}
	}
	t.counts[reason]++
	return t.counts[reason] == 1
}

func (t *srtpDropTally) snapshotAndReset() map[string]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.counts
	t.counts = nil
	return out
}

// noteInboundStream logs the first packet of every distinct inbound stream, with
// whether this call can route it at all.
func (m *CallManager) noteInboundStream(ssrc uint32, pt uint8, skip, hasSrtp, hasHandler bool) {
	m.extMu.Lock()
	if m.seenInbound == nil {
		m.seenInbound = map[uint64]bool{}
	}
	stream := uint64(ssrc)<<8 | uint64(pt)
	if m.seenInbound[stream] {
		m.extMu.Unlock()
		return
	}
	m.seenInbound[stream] = true
	m.extMu.Unlock()

	m.mu.Lock()
	expected := containsSsrc(m.peerSsrcs, ssrc)
	subscriptions := len(m.peerSsrcs)
	callID := ""
	if m.currentCall != nil {
		callID = m.currentCall.CallID
	}
	m.mu.Unlock()
	m.log.Info("inbound rtp stream seen",
		"call_id", callID, "ssrc", ssrc, "payload_type", pt,
		// expected=false means the SSRC we derived for this participant is not the
		// one they actually send on, so no receive key was ever registered for it.
		"expected", expected, "subscriptions", subscriptions,
		"declared_self", skip, "has_srtp", hasSrtp, "has_handler", hasHandler)
}

// noteStunResponse logs the first relay answer of each distinct kind, so an
// allocate the relay refuses is visible instead of silently dropped.
func (m *CallManager) noteStunResponse(data []byte) {
	info := transport.ParseStunResponse(data)
	if info == nil || info.StunClass == "indication" {
		return
	}
	kind := info.Method + "/" + info.StunClass + "/" + strconv.Itoa(info.ErrorCode)
	m.extMu.Lock()
	if m.seenStun == nil {
		m.seenStun = map[string]bool{}
	}
	if m.seenStun[kind] {
		m.extMu.Unlock()
		return
	}
	m.seenStun[kind] = true
	m.extMu.Unlock()

	if info.IsError {
		m.log.Warn("relay refused a stun request",
			"method", info.Method, "error_code", info.ErrorCode,
			"reason", info.ErrorReason)
		return
	}
	m.log.Info("relay stun response",
		"method", info.Method, "class", info.StunClass, "attributes", len(info.Attributes))
}

// noteUnroutable reports, once per reason, a packet the relay delivered that
// this call could not classify. Dropping these unlogged hides a dead media path
// behind a call that otherwise looks healthy.
func (m *CallManager) noteUnroutable(reason string, size int) {
	m.extMu.Lock()
	if m.seenUnroutable == nil {
		m.seenUnroutable = map[string]bool{}
	}
	if m.seenUnroutable[reason] {
		m.extMu.Unlock()
		return
	}
	m.seenUnroutable[reason] = true
	m.extMu.Unlock()
	m.log.Warn("relay packet not routable", "reason", reason, "bytes", size)
}

// keyDumpLimit bounds how many failing packets a single call reports.
const keyDumpLimit = 3

// dumpUnauthenticated records, under WACALLS_DUMP_KEYS, the group epoch and the
// first packets that failed to authenticate with it. Whether the receive key is
// derived from the right identity cannot be settled from the outside: the same
// symptom fits a wrong epoch, a wrong participant id and a wrong tag length, and
// only replaying the real bytes against each candidate separates them.
//
// It stays behind an env var because it prints key material.
func (m *CallManager) dumpUnauthenticated(data []byte) {
	if os.Getenv("WACALLS_DUMP_KEYS") == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keyDumps >= keyDumpLimit || m.group == nil || len(m.group.Epoch) == 0 {
		return
	}
	m.keyDumps++
	call := m.currentCall
	callID := ""
	if call != nil {
		callID = call.CallID
	}
	var devices []string
	if m.group.Roster != nil {
		for _, participant := range m.group.Roster.Participants {
			for _, device := range participant.Devices {
				if !device.JID.IsEmpty() {
					devices = append(devices, device.JID.String())
				}
			}
		}
	}
	m.log.Warn("unauthenticated packet dump",
		"call_id", callID,
		"epoch", hex.EncodeToString(m.group.Epoch),
		"packet", hex.EncodeToString(data),
		"roster_devices", strings.Join(devices, ","))
}

// noteInboundRTCP announces the first control packet of a call. Without inbound
// RTCP there is no round-trip or loss to report, and the quality panel stays on
// "measuring" for as long as the call lasts.
func (m *CallManager) noteInboundRTCP() {
	m.extMu.Lock()
	first := !m.sawInboundRTCP
	m.sawInboundRTCP = true
	m.extMu.Unlock()
	if first {
		m.log.Info("first inbound rtcp seen", "call_id", m.callIDForLog())
	}
}
