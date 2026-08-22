package appdata

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/engine"
	"wacalls/internal/voip/media"
)

type capture struct {
	mu   sync.Mutex
	pkts []*media.RtpPacket
}

func (c *capture) add(pkt *media.RtpPacket) {
	c.mu.Lock()
	c.pkts = append(c.pkts, pkt)
	c.mu.Unlock()
}

func (c *capture) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pkts)
}

func (c *capture) snapshot() []*media.RtpPacket {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*media.RtpPacket(nil), c.pkts...)
}

func newTestScope(cp *capture) *engine.CallScope {
	return &engine.CallScope{
		Log:             slog.Default(),
		CallID:          "CALL1",
		OwnDeviceJID:    "our:0@lid",
		PeerDeviceJID:   "peer:0@lid",
		Observer:        core.NopObserver{},
		SendRTP:         func(pkt *media.RtpPacket) error { cp.add(pkt); return nil },
		OnRTP:           func(uint8, func(*media.RtpPacket)) {},
		DeclareSelfSSRC: func(uint32) {},
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAppDataSendsTenCopiesOnOneStream(t *testing.T) {
	cp := &capture{}
	a := New()
	if err := a.Attach(newTestScope(cp)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer a.Detach()

	if err := a.SendReaction("\U0001F44D"); err != nil {
		t.Fatalf("SendReaction: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return cp.len() >= retransmitCount })

	sent := cp.snapshot()
	if len(sent) != retransmitCount {
		t.Fatalf("sent %d packets, want %d", len(sent), retransmitCount)
	}
	for i, pkt := range sent {
		if pkt.Header.PayloadType != core.PayloadTypeWhatsAppAppData {
			t.Errorf("packet %d payload type = %d, want %d", i, pkt.Header.PayloadType, core.PayloadTypeWhatsAppAppData)
		}
		if pkt.Header.Marker {
			t.Errorf("packet %d has the marker bit set; app-data never sets it", i)
		}
		if pkt.Header.Ssrc != sent[0].Header.Ssrc {
			t.Errorf("packet %d ssrc = %d, want all copies on one stream", i, pkt.Header.Ssrc)
		}
		// O payload e identico nas 10 copias: so o header avanca.
		if string(pkt.Payload) != string(sent[0].Payload) {
			t.Errorf("packet %d payload differs from the first copy", i)
		}
	}
	// Replica o remetente de referencia: seq comeca em 1, timestamp em 50 com passo 50.
	if sent[0].Header.SequenceNumber != 1 || sent[0].Header.Timestamp != timestampStep {
		t.Errorf("first packet seq/ts = %d/%d, want 1/%d",
			sent[0].Header.SequenceNumber, sent[0].Header.Timestamp, timestampStep)
	}
	if sent[1].Header.SequenceNumber != 2 || sent[1].Header.Timestamp != 2*timestampStep {
		t.Errorf("second packet seq/ts = %d/%d, want 2/%d",
			sent[1].Header.SequenceNumber, sent[1].Header.Timestamp, 2*timestampStep)
	}
}

func TestAppDataDeliversInboundReactionOnce(t *testing.T) {
	a := New()
	if err := a.Attach(newTestScope(&capture{})); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer a.Detach()

	var got []string
	a.OnPeerReaction(func(emoji string) { got = append(got, emoji) })

	payload := encodeReaction(1, "\U0001F44D")
	for range retransmitCount {
		a.handleInbound(&media.RtpPacket{
			Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppAppData, 1, 50, 9999),
			Payload: payload,
		})
	}

	if len(got) != 1 {
		t.Fatalf("delivered %d reactions, want 1", len(got))
	}
	if got[0] != "\U0001F44D" {
		t.Errorf("emoji = %q, want thumbs up", got[0])
	}
}

func TestAppDataIgnoresMalformedInbound(t *testing.T) {
	a := New()
	if err := a.Attach(newTestScope(&capture{})); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer a.Detach()

	called := false
	a.OnPeerReaction(func(string) { called = true })
	a.handleInbound(&media.RtpPacket{
		Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppAppData, 1, 50, 9999),
		Payload: []byte{0xff, 0xff, 0xff},
	})
	if called {
		t.Fatal("a malformed payload must not reach the callback")
	}
}

func TestAppDataSendAfterDetachFails(t *testing.T) {
	a := New()
	if err := a.Attach(newTestScope(&capture{})); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	a.Detach()
	if err := a.SendReaction("\U0001F44D"); err == nil {
		t.Fatal("SendReaction after Detach must fail")
	}
}

func TestAppDataDeclaresItsOwnSSRC(t *testing.T) {
	scope := newTestScope(&capture{})
	var declared []uint32
	scope.DeclareSelfSSRC = func(ssrc uint32) { declared = append(declared, ssrc) }

	a := New()
	if err := a.Attach(scope); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer a.Detach()

	want := media.GenerateSecureSsrc("CALL1", "our:0@lid", core.SsrcCounterAppData)
	if len(declared) != 1 || declared[0] != want {
		t.Fatalf("declared = %v, want [%d]; without this the relay echo of our own stream is processed as the peer's", declared, want)
	}
}

func TestAppDataRegistersTheAppDataPayloadType(t *testing.T) {
	scope := newTestScope(&capture{})
	var registered []uint8
	scope.OnRTP = func(pt uint8, _ func(*media.RtpPacket)) { registered = append(registered, pt) }

	a := New()
	if err := a.Attach(scope); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer a.Detach()

	if len(registered) != 1 || registered[0] != core.PayloadTypeWhatsAppAppData {
		t.Fatalf("registered payload types = %v, want [%d]", registered, core.PayloadTypeWhatsAppAppData)
	}
}
