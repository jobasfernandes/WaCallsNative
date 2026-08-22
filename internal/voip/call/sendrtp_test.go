package call

import (
	"log/slog"
	"testing"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/engine"
	"wacalls/internal/voip/media"
)

func TestSendRTPProtectsAndBroadcasts(t *testing.T) {
	k1, k2 := km(1), km(9)
	m := NewCallManager(fakeSock{}, slog.Default())
	var sent []byte
	m.relay = &fakeRelay{onData: func(b []byte) { sent = b }}
	m.srtp = engine.NewSrtpManager(k1, k2, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)

	pkt := &media.RtpPacket{
		Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppAppData, 1, 50, 4242),
		Payload: []byte{0x01, 0x02, 0x03},
	}
	if err := m.sendRTP(pkt); err != nil {
		t.Fatalf("sendRTP: %v", err)
	}
	if len(sent) == 0 {
		t.Fatal("sendRTP must broadcast the protected packet")
	}
	if got := sent[1] & 0x7f; got != core.PayloadTypeWhatsAppAppData {
		t.Fatalf("payload type on the wire = %d, want %d", got, core.PayloadTypeWhatsAppAppData)
	}
	if got := media.RTPSsrc(sent); got != 4242 {
		t.Fatalf("ssrc on the wire = %d, want 4242", got)
	}
}

func TestSendRTPWithoutMediaReturnsError(t *testing.T) {
	m := NewCallManager(fakeSock{}, slog.Default())
	m.relay = &fakeRelay{}
	pkt := &media.RtpPacket{
		Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppAppData, 1, 50, 1),
		Payload: []byte{0x01},
	}
	if err := m.sendRTP(pkt); err == nil {
		t.Fatal("sendRTP must fail when SRTP is not established")
	}
}
