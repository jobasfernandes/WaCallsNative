package call

import (
	"time"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
	"wacalls/internal/voip/media/h264"
	"wacalls/internal/voip/wanode"
)

// videoAssembler turns depacketized H.264 NAL units into Annex-B access units, emitting one when
// the RTP marker bit closes a frame, with a duration derived from the 90 kHz RTP timestamp delta.
type videoAssembler struct {
	depack h264.Depacketizer
	au     []byte
	lastTs uint32
	tsInit bool

	// Diagnostic window: frames and bytes emitted since statStart.
	framesRx  int
	bytesRx   int
	idrRx     int
	statStart time.Time
}

func (a *videoAssembler) feed(seq uint16, payload []byte, ts uint32, marker bool) ([]byte, time.Duration) {
	nals, err := a.depack.Depacketize(seq, payload)
	if err != nil {
		return nil, 0
	}
	for _, nal := range nals {
		a.au = append(a.au, h264.AnnexB([][]byte{nal})...)
	}
	if !marker || len(a.au) == 0 {
		return nil, 0
	}
	au := a.au
	a.au = nil
	var dur time.Duration
	if a.tsInit {
		dur = time.Duration(ts-a.lastTs) * time.Second / 90000
	}
	if dur <= 0 || dur > time.Second {
		dur = 66 * time.Millisecond // fallback ~15 fps
	}
	a.lastTs = ts
	a.tsInit = true
	return au, dur
}

// parseCVORotation reads the WhatsApp CVO (video orientation) from the clear RTP one-byte-header
// extension (profile 0xBEDE); returns rotation in degrees, or -1 when absent. The RTP header and
// extension are not encrypted by SRTP, so it reads the raw packet.
func parseCVORotation(data []byte) int {
	if len(data) < 12 || (data[0]>>4)&1 == 0 {
		return -1
	}
	off := 12 + int(data[0]&0x0f)*4
	if len(data) < off+4 {
		return -1
	}
	extLen := (int(data[off+2])<<8 | int(data[off+3])) * 4
	start := off + 4
	end := min(start+extLen, len(data))
	ext := data[start:end]
	for i := 0; i < len(ext); {
		b := ext[i]
		if b == 0 { // padding
			i++
			continue
		}
		id := b >> 4
		l := int(b&0x0f) + 1
		i++
		if i+l > len(ext) {
			break
		}
		if l == 1 && id != 15 {
			return int(ext[i]&0x03) * 90
		}
		i += l
	}
	return -1
}

// handleVideoPacket processes an inbound non-audio (H.264) RTP packet from the relay. It keeps a
// dedicated SRTP context (separate ROC from audio) keyed off the same per-JID recv material, locks
// onto the first stream whose decrypted payload looks like H.264, reassembles Annex-B access units,
// and hands each completed unit to OnPeerVideo. The peer's CVO orientation (clear RTP extension) is
// forwarded to OnPeerVideoRotation when it changes.
func (m *CallManager) handleVideoPacket(data []byte, ssrc uint32) {
	m.mu.Lock()
	m.videoRxSeen++
	firstRx := m.videoRxSeen == 1
	if m.recvKM.MasterKey == nil {
		m.mu.Unlock()
		return
	}
	if m.videoSrtp == nil {
		ctx, err := media.NewSrtpContext(m.recvKM, core.SRTPRecvAuthTagLen)
		if err != nil {
			m.mu.Unlock()
			return
		}
		m.videoSrtp = ctx
		m.videoAsm = &videoAssembler{}
	}
	if m.videoSsrc != 0 && ssrc != m.videoSsrc {
		m.mu.Unlock()
		return
	}
	locking := m.videoSsrc == 0
	vctx := m.videoSrtp
	asm := m.videoAsm
	callID := ""
	if m.currentCall != nil {
		callID = m.currentCall.CallID
	}
	rotChanged := false
	if deg := parseCVORotation(data); deg >= 0 && deg != m.videoRotation {
		m.videoRotation = deg
		rotChanged = true
	}
	rotation := m.videoRotation
	m.mu.Unlock()

	if rotChanged && m.OnPeerVideoRotation != nil {
		m.OnPeerVideoRotation(callID, rotation)
	}

	pkt, err := vctx.Unprotect(data)
	if firstRx {
		// Diagnostic: proves whether the peer's video reaches us at all (relay/subscription) and,
		// if so, whether our video SRTP context decrypts it to a plausible H.264 NAL (keying).
		plausible := err == nil && pkt != nil && len(pkt.Payload) > 0 && h264.IsPlausibleNALHeader(pkt.Payload[0])
		m.log.Info("video rx first packet", "call_id", callID, "ssrc", ssrc, "pt", data[1]&0x7f,
			"srtp_ok", err == nil, "plausible_nal", plausible)
	}
	if err != nil || len(pkt.Payload) == 0 {
		return
	}
	if locking {
		if !h264.IsPlausibleNALHeader(pkt.Payload[0]) {
			return
		}
		m.mu.Lock()
		if m.videoSsrc == 0 {
			m.videoSsrc = ssrc
			m.videoRecvPT = data[1] & 0x7f
			// Subscribe the peer video SSRC at stream layer 1 so the relay delivers the full
			// video layer instead of the throttled base layer (low quality otherwise).
			m.relay.SetPeerVideoSsrc(ssrc)
			m.mu.Unlock()
			go m.relay.ResendSubscriptions()
			m.log.Info("video stream locked", "call_id", callID, "ssrc", ssrc, "pt", data[1]&0x7f)
		} else if m.videoSsrc != ssrc {
			m.mu.Unlock()
			return
		} else {
			m.mu.Unlock()
		}
	}

	au, dur := asm.feed(pkt.Header.SequenceNumber, pkt.Payload, pkt.Header.Timestamp, pkt.Header.Marker)
	if au == nil {
		return
	}
	m.logVideoRx(callID, asm, au)
	if m.OnPeerVideo != nil {
		m.OnPeerVideo(callID, au, dur)
	}
}

// logVideoRx accumulates per-frame stats and emits a ~5s summary so we can tell a low-bitrate
// downlink (small frames at a normal rate) from frame loss (frequent keyframe waits). Metadata
// only, never the payload.
func (m *CallManager) logVideoRx(callID string, asm *videoAssembler, au []byte) {
	now := time.Now()
	if asm.statStart.IsZero() {
		asm.statStart = now
	}
	asm.framesRx++
	asm.bytesRx += len(au)
	if annexBHasIDR(au) {
		asm.idrRx++
	}
	elapsed := now.Sub(asm.statStart)
	if elapsed < 5*time.Second {
		return
	}
	secs := elapsed.Seconds()
	m.log.Info("video rx summary", "call_id", callID,
		"fps", float64(asm.framesRx)/secs,
		"kbps", float64(asm.bytesRx*8)/secs/1000,
		"avg_frame_bytes", asm.bytesRx/max(asm.framesRx, 1),
		"idr", asm.idrRx,
		"keyframe_waits", asm.depack.KeyframeWaits)
	asm.framesRx, asm.bytesRx, asm.idrRx = 0, 0, 0
	asm.statStart = now
}

func annexBHasIDR(au []byte) bool {
	for i := 0; i+4 < len(au); i++ {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 0 && au[i+3] == 1 {
			if au[i+4]&0x1f == 5 {
				return true
			}
		}
	}
	return false
}

func (m *CallManager) resetVideoRecvLocked() {
	m.videoSrtp = nil
	m.videoSsrc = 0
	m.videoAsm = nil
	m.videoRotation = 0
	m.videoRecvPT = 0
	m.videoRxSeen = 0
	m.videoSendSrtp = nil
	m.videoSelfSsrc = 0
	m.videoSendInit = false
	m.videoSendSeq = 0
	m.videoTxFrames, m.videoTxBytes, m.videoTxKeyframes = 0, 0, 0
	m.videoTxStart = time.Time{}
}

// SendPeerVideo forwards one browser-encoded, already-RFC-6184-packetized H.264 RTP payload to
// the WhatsApp peer. It rewrites the RTP header (our video SSRC at counter 2, PT 97 or the locked
// downlink PT, our own sequence) and re-encrypts under the per-JID send key in a dedicated SRTP
// context. Timestamp and marker come straight from the browser. No-op unless the call carries
// video and the relay is up. The first frame lazily declares our video SSRC to the relay.
func (m *CallManager) SendPeerVideo(payload []byte, ts uint32, marker bool) {
	if len(payload) == 0 {
		return
	}
	m.mu.Lock()
	if !m.localVideo || m.sendKM.MasterKey == nil || !m.relay.HasConnection() {
		m.mu.Unlock()
		return
	}
	if !m.videoSendInit {
		callID, ourJid := "", m.videoOurDeviceJid
		if m.currentCall != nil {
			callID = m.currentCall.CallID
		}
		if ourJid == "" {
			var participants []string
			if m.currentCall != nil && m.currentCall.RelayData != nil {
				participants = m.currentCall.RelayData.ParticipantJids
			}
			ourJid = ensureDeviceJid(findOurDevice(participants, wanode.CleanJID(m.ownCredJid()), m.ownCredJid()))
			m.videoOurDeviceJid = ourJid
		}
		ctx, err := media.NewSrtpContext(m.sendKM, core.SRTPSendAuthTagLen)
		if err != nil {
			m.mu.Unlock()
			m.log.Error("video send srtp init failed", "err", err)
			return
		}
		m.videoSendSrtp = ctx
		m.videoSelfSsrc = media.GenerateSecureSsrc(callID, ourJid, 2)
		m.videoSendInit = true
		// Mark our own video SSRC as self so the inbound NAL-sniff never locks onto our echo.
		m.declareSelfSSRC(m.videoSelfSsrc)
		m.relay.SetVideoSsrc(m.videoSelfSsrc)
		go m.relay.ResendSubscriptions()
		m.log.Info("video uplink started", "call_id", callID, "ssrc", m.videoSelfSsrc)
	}
	var pt uint8 = core.PayloadTypeWhatsAppH264
	if m.videoRecvPT != 0 {
		pt = m.videoRecvPT
	}
	hdr := media.NewRtpHeader(pt, m.videoSendSeq, ts, m.videoSelfSsrc)
	hdr.Marker = marker
	m.videoSendSeq++
	ctx := m.videoSendSrtp
	callID := ""
	if m.currentCall != nil {
		callID = m.currentCall.CallID
	}
	m.mu.Unlock()

	protected, err := ctx.Protect(&media.RtpPacket{Header: hdr, Payload: payload})
	if err != nil {
		m.log.Debug("video srtp protect error", "err", err)
		return
	}
	m.relay.Broadcast(protected)
	m.logVideoTx(callID, len(payload), marker, payloadIsKeyframeNAL(payload))
}

// payloadIsKeyframeNAL reports whether an outbound H.264 RTP payload starts an IDR or SPS NAL
// (a keyframe). Handles single-NAL and the start packet of an FU-A fragment. Diagnostic only:
// it tells us how often the browser encoder is emitting keyframes (peer resync points).
func payloadIsKeyframeNAL(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	nalType := payload[0] & 0x1f
	if nalType == 28 { // FU-A: original type is in the FU header, count only the start fragment
		if len(payload) < 2 || payload[1]&0x80 == 0 {
			return false
		}
		nalType = payload[1] & 0x1f
	}
	return nalType == 5 || nalType == 7 // IDR slice or SPS
}

// logVideoTx emits a ~5s uplink summary (frames, kbps, keyframes) mirroring logVideoRx. Metadata
// only. key_nals is how many IDR/SPS NALs we sent in the window: too few means the peer has no
// resync point after packet loss, which reads as an intermittent freeze on its side.
func (m *CallManager) logVideoTx(callID string, n int, marker, keyframe bool) {
	m.mu.Lock()
	now := time.Now()
	if m.videoTxStart.IsZero() {
		m.videoTxStart = now
	}
	m.videoTxBytes += n
	if marker {
		m.videoTxFrames++
	}
	if keyframe {
		m.videoTxKeyframes++
	}
	elapsed := now.Sub(m.videoTxStart)
	if elapsed < 5*time.Second {
		m.mu.Unlock()
		return
	}
	secs := elapsed.Seconds()
	frames, bytes, keys := m.videoTxFrames, m.videoTxBytes, m.videoTxKeyframes
	m.videoTxFrames, m.videoTxBytes, m.videoTxKeyframes = 0, 0, 0
	m.videoTxStart = now
	m.mu.Unlock()
	m.log.Info("video tx summary", "call_id", callID,
		"fps", float64(frames)/secs, "kbps", float64(bytes*8)/secs/1000, "key_nals", keys)
}
