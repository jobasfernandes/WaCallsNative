package transport

import "slices"

// streamDescriptorPlan maps each of the nine relay stream slots to the
// participant and layer the allocate announces for it.
var streamDescriptorPlan = [9]struct {
	participant uint32
	layer       uint32
}{
	{0, 0}, {0, 1}, {0, 2},
	{1, 0}, {1, 1}, {1, 2},
	{2, 0}, {2, 1}, {2, 2},
}

// hbhFECParticipantOffset is where the hop-by-hop FEC descriptors start: the
// captured client announces them as participants 3 and 4, both on layer 3.
const (
	hbhFECParticipantOffset = 3
	hbhFECLayer             = 3
)

// NormalizeParticipantPIDs sorts and deduplicates the connected remote PIDs and
// drops zero, which is the local participant and is never subscribed to.
func NormalizeParticipantPIDs(pids []uint32) []uint32 {
	var out []uint32
	for _, pid := range pids {
		if pid == 0 || slices.Contains(out, pid) {
			continue
		}
		out = append(out, pid)
	}
	slices.Sort(out)
	return out
}

// BuildGroupSenderSubscriptions builds the four subscription groups in the order
// the captured client emits them: primary video (flagged as video), secondary
// video (no participants), audio, and app-data.
func BuildGroupSenderSubscriptions(streamSSRCs [9]uint32, appDataSSRC uint32, pids []uint32) []byte {
	return concat(
		buildSenderSubscription(streamSSRCs[3:6], pids, true),
		buildSenderSubscription(streamSSRCs[6:9], nil, false),
		buildSenderSubscription(streamSSRCs[0:3], pids, false),
		buildSenderSubscription([]uint32{appDataSSRC}, pids, false),
	)
}

func buildSenderSubscription(ssrcs []uint32, pids []uint32, video bool) []byte {
	// The SSRCs are a packed repeated field: bare varints with no per-item tag.
	var packed []byte
	for _, ssrc := range ssrcs {
		if ssrc != 0 {
			packed = append(packed, encodeVarint(uint64(ssrc))...)
		}
	}
	subscription := encodeProtobufLengthDelimited(1, packed)
	for _, pid := range pids {
		participant := encodeProtobufVarintField(1, uint64(pid))
		if video {
			participant = append(participant, encodeProtobufVarintField(2, 1)...)
		}
		subscription = append(subscription, encodeProtobufLengthDelimited(2, participant)...)
	}
	return encodeProtobufLengthDelimited(1, encodeProtobufLengthDelimited(1, subscription))
}

// BuildGroupReceiverSubscriptions selects which remote participants to receive.
// It carries PIDs only, never SSRCs.
func BuildGroupReceiverSubscriptions(pids []uint32) []byte {
	var out []byte
	for _, pid := range pids {
		out = append(out, encodeProtobufLengthDelimited(2, encodeProtobufVarintField(1, uint64(pid)))...)
	}
	return out
}

// BuildGroupStreamDescriptors builds the nine local stream descriptors, plus the
// two hop-by-hop FEC descriptors when they are supplied. The captured client only
// sends the FEC pair once more than one remote participant is connected.
func BuildGroupStreamDescriptors(streamSSRCs [9]uint32, hbhFEC [2]uint32) []byte {
	var out []byte
	for i, ssrc := range streamSSRCs {
		if ssrc == 0 {
			continue
		}
		plan := streamDescriptorPlan[i]
		var inner []byte
		// Participant and layer zero are implicit and left off the wire.
		if plan.participant != 0 {
			inner = append(inner, encodeProtobufVarintField(1, uint64(plan.participant))...)
		}
		if plan.layer != 0 {
			inner = append(inner, encodeProtobufVarintField(2, uint64(plan.layer))...)
		}
		inner = append(inner, encodeProtobufVarintField(3, uint64(ssrc))...)
		out = append(out, encodeProtobufLengthDelimited(1, inner)...)
	}
	for i, ssrc := range hbhFEC {
		if ssrc == 0 {
			continue
		}
		inner := concat(
			encodeProtobufVarintField(1, uint64(i+hbhFECParticipantOffset)),
			encodeProtobufVarintField(2, hbhFECLayer),
			encodeProtobufVarintField(3, uint64(ssrc)),
		)
		out = append(out, encodeProtobufLengthDelimited(1, inner)...)
	}
	return out
}
