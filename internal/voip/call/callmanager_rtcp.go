package call

import (
	"encoding/binary"
	"fmt"
	"time"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
)

// RTCP transmit cadence, matching the official WhatsApp client captured on the wire: compact 208
// roughly every second, the sender-report compound every 1.5s, compact 209 every 3s.
const (
	rtcp208Interval  = time.Second
	rtcpSRInterval   = 1500 * time.Millisecond
	rtcp209Interval  = 3 * time.Second
	rtcpPrstInterval = time.Second
)

// prstAdvertisedBandwidth is the receive bandwidth (bps) our PRST feedback advertises to the
// WhatsApp relay so it stops throttling the video downlink to the base layer.
const prstAdvertisedBandwidth = 1000000

// maybeStartRtcpTxLocked starts the RTCP transmit loop once the media keys and call are ready.
// Idempotent: a second call while the loop runs is a no-op, so it is safe to invoke from every
// path that transitions the call to active (including reconnection). Caller holds m.mu.
func (m *CallManager) maybeStartRtcpTxLocked() {
	if m.rtcpTxStop != nil || m.sendSrtcp == nil || m.currentCall == nil {
		return
	}
	stop := make(chan struct{})
	m.rtcpTxStop = stop
	done := m.observer.TrackGoroutine()
	go func() {
		defer done()
		m.runRtcpTx(stop)
	}()
	m.log.Info("rtcp tx started", "call_id", m.currentCall.CallID, "ssrc", m.selfSsrc)
}

func (m *CallManager) runRtcpTx(stop chan struct{}) {
	t208 := time.NewTicker(m.rtcp208Tick)
	tSR := time.NewTicker(m.rtcpSRTick)
	t209 := time.NewTicker(m.rtcp209Tick)
	tPrst := time.NewTicker(rtcpPrstInterval)
	defer t208.Stop()
	defer tSR.Stop()
	defer t209.Stop()
	defer tPrst.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t208.C:
			m.emitRtcp208()
		case <-tSR.C:
			m.emitRtcpSR()
		case <-t209.C:
			m.emitRtcp209()
		case <-tPrst.C:
			m.emitPrst()
		}
	}
}

// emitPrst sends the proprietary PRST feedback advertising a high receive bandwidth, so the
// WhatsApp relay stops throttling the video downlink. Video calls only; a no-op otherwise.
func (m *CallManager) emitPrst() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendSrtcp == nil || m.currentCall == nil || m.currentCall.MediaType != core.CallMediaTypeVideo {
		return
	}
	m.protectAndBroadcastLocked(buildPrstFeedback(m.selfSsrc, prstAdvertisedBandwidth))
}

// buildPrstFeedback builds the WhatsApp PRST RTCP feedback packet (PSFB FMT=15, PT=206) whose FCI
// carries the advertised bandwidth in the protobuf {1:{1:bandwidth}, 4:1}. bandwidth must fit a
// 3-byte varint so the packet stays 4-byte aligned.
func buildPrstFeedback(selfSsrc uint32, bandwidth uint32) []byte {
	nested := append([]byte{0x08}, appendVarint(nil, uint64(bandwidth))...)
	pb := append([]byte{0x0a, byte(len(nested))}, nested...)
	pb = append(pb, 0x20, 0x01)
	fci := append([]byte{'P', 'R', 'S', 'T', 0x00, byte(len(pb))}, pb...)

	buf := make([]byte, 0, 12+len(fci)+3)
	buf = append(buf, 0x8f, 0xce, 0x00, 0x00) // V=2, FMT=15, PT=206; length filled below
	var s [4]byte
	binary.BigEndian.PutUint32(s[:], selfSsrc)
	buf = append(buf, s[:]...)    // sender SSRC = us
	buf = append(buf, 0, 0, 0, 0) // media SSRC = 0 (app-specific)
	buf = append(buf, fci...)
	for len(buf)%4 != 0 {
		buf = append(buf, 0x00)
	}
	binary.BigEndian.PutUint16(buf[2:4], uint16(len(buf)/4-1))
	return buf
}

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func (m *CallManager) nextSrtcpIndexLocked() uint32 {
	idx := m.srtcpTxIndex
	m.srtcpTxIndex++
	return idx
}

// The emit* methods hold m.mu across build, protect and broadcast (like sendAudioFrame) so the
// send SRTCP context and relay cannot be torn down by cleanupMedia mid-send. recvStats.mu is a
// leaf lock (never re-enters m.mu), so taking it under m.mu is deadlock-free.

func (m *CallManager) emitRtcp208() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendSrtcp == nil {
		return
	}
	pkt := media.BuildCompact208(m.selfSsrc, m.lastRtpTs)
	m.protectAndBroadcastLocked(pkt[:])
}

func (m *CallManager) emitRtcp209() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendSrtcp == nil {
		return
	}
	pkt := media.BuildCompact209(m.selfSsrc)
	m.protectAndBroadcastLocked(pkt[:])
}

func (m *CallManager) emitRtcpSR() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendSrtcp == nil {
		return
	}
	stats := media.RTCPSenderStats{
		PacketsSent:  m.rtpPacketsSent,
		OctetsSent:   m.rtpOctetsSent,
		RtpTimestamp: m.lastRtpTs,
	}
	now := uint64(time.Now().UnixMilli())
	var rb *media.RTCPReportBlock
	if peer := firstSsrc(m.peerSsrcs); m.recvStats != nil && peer != 0 {
		b := m.recvStats.ReportBlock(peer, now)
		rb = &b
	}
	pkt := media.BuildRTCPCompound(m.selfSsrc, stats, rb, m.rtcpCName, now)
	m.protectAndBroadcastLocked(pkt)
}

func (m *CallManager) protectAndBroadcastLocked(rtcp []byte) {
	protected, err := m.sendSrtcp.Protect(rtcp, m.nextSrtcpIndexLocked())
	if err != nil {
		m.log.Debug("srtcp protect error", "err", err)
		return
	}
	m.relay.Broadcast(protected)
}

func rtcpCName(ssrc uint32) string {
	return fmt.Sprintf("%08x@wacalls", ssrc)
}
