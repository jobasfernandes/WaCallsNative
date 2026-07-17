package call

import (
	"time"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
	"wacalls/internal/voip/media/h264"
)

// videoAssembler turns depacketized H.264 NAL units into Annex-B access units, emitting one when
// the RTP marker bit closes a frame, with a duration derived from the 90 kHz RTP timestamp delta.
type videoAssembler struct {
	depack h264.Depacketizer
	au     []byte
	lastTs uint32
	tsInit bool
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
	end := start + extLen
	if end > len(data) {
		end = len(data)
	}
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
			m.log.Info("video stream locked", "call_id", callID, "ssrc", ssrc, "pt", data[1]&0x7f)
		} else if m.videoSsrc != ssrc {
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
	}

	au, dur := asm.feed(pkt.Header.SequenceNumber, pkt.Payload, pkt.Header.Timestamp, pkt.Header.Marker)
	if au == nil {
		return
	}
	if m.OnPeerVideo != nil {
		m.OnPeerVideo(callID, au, dur)
	}
}

func (m *CallManager) resetVideoRecvLocked() {
	m.videoSrtp = nil
	m.videoSsrc = 0
	m.videoAsm = nil
	m.videoRotation = 0
}
