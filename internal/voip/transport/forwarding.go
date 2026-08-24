package transport

const groupForwardingMarker byte = 0x09

// UnwrapGroupForwardingPacket strips the forwarding header the relay prepends in
// multi-participant mode.
//
// The header grows with the subtype, two bytes per step: subtype 2 is 8 bytes,
// 3 is 10, and so on. A capture of the official client confirms the progression
// across every packet the relay delivers. A fixed table of the few subtypes that
// happened to carry audio rejected the rest, and RTCP went with them, leaving
// the call unable to report latency or loss.
//
// Some subtypes arrive as header only. Those carry no media and are not errors.
// A packet without the marker is returned unchanged, which is the 1:1 path.
func UnwrapGroupForwardingPacket(data []byte) (payload []byte, wrapped, valid bool) {
	if len(data) == 0 || data[0] != groupForwardingMarker {
		return data, false, true
	}
	if len(data) < 2 {
		return nil, true, false
	}
	headerBytes := 2*int(data[1]) + 4
	if len(data) < headerBytes {
		return nil, true, false
	}
	if len(data) == headerBytes {
		return nil, true, true
	}
	// Anything past the header must be an RTP or RTCP packet; the version field
	// is what separates a real payload from a header we misread.
	if len(data) < headerBytes+2 || data[headerBytes]>>6 != 2 {
		return nil, true, false
	}
	return data[headerBytes:], true, true
}
