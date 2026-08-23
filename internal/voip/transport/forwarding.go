package transport

const groupForwardingMarker byte = 0x09

// UnwrapGroupForwardingPacket strips the forwarding header the relay prepends in
// multi-participant mode. The header length is encoded in the subtype byte, and
// what remains must be RTP version 2. A packet without the marker is returned
// unchanged, which is the 1:1 path.
func UnwrapGroupForwardingPacket(data []byte) (payload []byte, wrapped, valid bool) {
	if len(data) == 0 || data[0] != groupForwardingMarker {
		return data, false, true
	}
	if len(data) < 2 {
		return nil, true, false
	}
	var headerBytes int
	switch data[1] {
	case 2:
		headerBytes = 8
	case 4:
		headerBytes = 12
	case 7:
		headerBytes = 18
	default:
		return nil, true, false
	}
	if len(data) < headerBytes+12 || data[headerBytes]>>6 != 2 {
		return nil, true, false
	}
	return data[headerBytes:], true, true
}
