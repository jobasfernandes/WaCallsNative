package call

import (
	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
	"wacalls/internal/voip/media/h264"
)

// videoAssembler turns a sequence of depacketized H.264 NAL units into Annex-B access units,
// emitting one when the RTP marker bit closes a frame.
type videoAssembler struct {
	depack h264.Depacketizer
	au     []byte
}

func (a *videoAssembler) feed(seq uint16, payload []byte, marker bool) []byte {
	nals, err := a.depack.Depacketize(seq, payload)
	if err != nil {
		return nil
	}
	for _, nal := range nals {
		a.au = append(a.au, h264.AnnexB([][]byte{nal})...)
	}
	if marker && len(a.au) > 0 {
		au := a.au
		a.au = nil
		return au
	}
	return nil
}

// handleVideoPacket processes an inbound non-audio (H.264) RTP packet from the relay. It keeps a
// dedicated SRTP context (separate ROC from audio) keyed off the same per-JID recv material, locks
// onto the first stream whose decrypted payload looks like H.264, reassembles Annex-B access units,
// and hands each completed unit to OnPeerVideo.
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
	m.mu.Unlock()

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

	au := asm.feed(pkt.Header.SequenceNumber, pkt.Payload, pkt.Header.Marker)
	if au == nil {
		return
	}
	if m.OnPeerVideo != nil {
		m.OnPeerVideo(callID, au)
	}
}

func (m *CallManager) resetVideoRecvLocked() {
	m.videoSrtp = nil
	m.videoSsrc = 0
	m.videoAsm = nil
}
