package media

import "encoding/binary"

// RTCPGroupReportExtension is the opaque eight-byte suffix a group-call audio
// reception report carries. Its meaning is unknown; it is echoed as captured.
type RTCPGroupReportExtension [8]byte

// BuildGroupSenderReport builds the sender report a group call sends: one
// reception report block plus the opaque extension, and no SDES. With no report
// block there is nothing to extend, so it degenerates to the plain SR.
func BuildGroupSenderReport(
	local uint32,
	s RTCPSenderStats,
	rb *RTCPReportBlock,
	extension RTCPGroupReportExtension,
	nowMs uint64,
) []byte {
	sender := BuildSenderReportWithBlock(local, s, rb, nowMs)
	if rb == nil {
		return sender
	}
	out := append(sender, extension[:]...)
	binary.BigEndian.PutUint16(out[2:4], uint16(len(out)/4-1))
	return out
}
