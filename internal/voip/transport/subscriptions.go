package transport

func encodeVarint(value uint64) []byte {
	var out []byte
	v := value
	for v > 0x7f {
		out = append(out, byte((v&0x7f)|0x80))
		v >>= 7
	}
	out = append(out, byte(v&0x7f))
	return out
}

func encodeProtobufVarintField(fieldNumber int, value uint64) []byte {
	tag := encodeVarint(uint64(fieldNumber << 3))
	return append(tag, encodeVarint(value)...)
}

func encodeProtobufLengthDelimited(fieldNumber int, data []byte) []byte {
	tag := encodeVarint(uint64((fieldNumber << 3) | 2))
	out := append(tag, encodeVarint(uint64(len(data)))...)
	return append(out, data...)
}

type SubEntry struct {
	SSRC        uint32
	StreamLayer int
	PayloadType int
}

func BuildSenderSubscriptions(entries ...SubEntry) []byte {
	var out []byte
	for _, e := range entries {
		inner := concat(
			encodeProtobufVarintField(3, uint64(e.SSRC)),
			encodeProtobufVarintField(5, uint64(e.StreamLayer)),
			encodeProtobufVarintField(6, uint64(e.PayloadType)),
		)
		out = append(out, encodeProtobufLengthDelimited(1, inner)...)
	}
	return out
}

func BuildSSRCSubscriptionList(selfSsrcs, peerSsrcs []uint32, selfPid, peerPid int) []byte {
	var entries [][]byte
	for _, ssrc := range selfSsrcs {
		if ssrc == 0 {
			continue
		}
		inner := concat(
			encodeProtobufVarintField(1, uint64(selfPid)),
			encodeProtobufVarintField(2, 1),
			encodeProtobufVarintField(3, uint64(ssrc)),
		)
		entries = append(entries, encodeProtobufLengthDelimited(1, inner))
	}
	for _, ssrc := range peerSsrcs {
		if ssrc == 0 {
			continue
		}
		inner := concat(
			encodeProtobufVarintField(1, uint64(peerPid)),
			encodeProtobufVarintField(2, 1),
			encodeProtobufVarintField(3, uint64(ssrc)),
		)
		entries = append(entries, encodeProtobufLengthDelimited(1, inner))
	}
	return concat(entries...)
}
