package call

import (
	"log/slog"
	"testing"

	"wacalls/internal/voip/core"
)

func TestOnRelayDataForwardsInboundPLI(t *testing.T) {
	m := NewCallManager(fakeSock{}, slog.Default())
	m.currentCall = NewIncomingCall("CALLPLI0001", "peer@lid", "creator@lid", "", core.CallMediaTypeVideo)
	fired := make(chan string, 1)
	m.OnPeerKeyframeRequest = func(callID string) { fired <- callID }

	// PSFB PLI: V=2,P=0,FMT=1 -> 0x81 ; PT=206 ; length=2 ; sender ssrc ; media ssrc.
	pli := []byte{0x81, 206, 0x00, 0x02, 0, 0, 0, 1, 0, 0, 0, 2}
	m.onRelayData(pli)

	select {
	case id := <-fired:
		if id != "CALLPLI0001" {
			t.Fatalf("callID = %q", id)
		}
	default:
		t.Fatal("OnPeerKeyframeRequest did not fire for inbound PLI")
	}
}

func TestOnRelayDataIgnoresNonPLIRtcp(t *testing.T) {
	m := NewCallManager(fakeSock{}, slog.Default())
	m.currentCall = NewIncomingCall("CALLPLI0002", "peer@lid", "creator@lid", "", core.CallMediaTypeVideo)
	fired := false
	m.OnPeerKeyframeRequest = func(string) { fired = true }

	// A receiver report (PT 201), not a keyframe request.
	rr := []byte{0x80, 201, 0x00, 0x01, 0, 0, 0, 1}
	m.onRelayData(rr)

	if fired {
		t.Fatal("a receiver report must not trigger a keyframe request")
	}
}
