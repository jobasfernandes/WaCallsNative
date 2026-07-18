package call

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
)

// capRelay records outbound broadcasts and video-SSRC bookkeeping without a real transport.
// It is mutex-guarded because SendPeerVideo declares the video SSRC via a goroutine
// (go ResendSubscriptions), which races the test's reads otherwise.
type capRelay struct {
	fakeRelay
	mu        sync.Mutex
	sent      [][]byte
	videoSsrc uint32
	resend    int
}

func (r *capRelay) Broadcast(data []byte) {
	r.mu.Lock()
	r.sent = append(r.sent, append([]byte(nil), data...))
	r.mu.Unlock()
}

func (r *capRelay) SetVideoSsrc(ssrc uint32) {
	r.mu.Lock()
	r.videoSsrc = ssrc
	r.mu.Unlock()
}

func (r *capRelay) ResendSubscriptions() {
	r.mu.Lock()
	r.resend++
	r.mu.Unlock()
}

func (r *capRelay) snapshot() (sent [][]byte, videoSsrc uint32, resend int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.sent...), r.videoSsrc, r.resend
}

// waitResend polls until the async ResendSubscriptions goroutine has run, or fails.
func (r *capRelay) waitResend(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, resend := r.snapshot(); resend > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("ResendSubscriptions was never called for the video uplink")
}

func newVideoSendCM(t *testing.T, relay *capRelay) (*CallManager, core.SrtpKeyingMaterial) {
	t.Helper()
	callID := "TESTCALL0001"
	ourJid := "10@s.whatsapp.net"
	key := media.GenerateCallKey()
	sendKM, err := media.DerivePerJidSrtpKey(key, ourJid)
	if err != nil {
		t.Fatalf("send key: %v", err)
	}
	// The peer decrypts our video with keying derived from OUR device jid (same as our send key).
	peerRecvKM, err := media.DerivePerJidSrtpKey(key, ourJid)
	if err != nil {
		t.Fatalf("peer recv key: %v", err)
	}
	m := NewCallManager(fakeSock{}, slog.Default())
	m.relay = relay
	m.localVideo = true
	m.sendKM = sendKM
	m.currentCall = NewIncomingCall(callID, "peer@lid", "creator@lid", "", core.CallMediaTypeVideo)
	m.videoOurDeviceJid = ourJid
	return m, peerRecvKM
}

func TestSendPeerVideoBuildsHeaderAndProtects(t *testing.T) {
	relay := &capRelay{}
	m, peerRecvKM := newVideoSendCM(t, relay)

	m.SendPeerVideo([]byte{0x65, 0x11, 0x22}, 9000, false)
	m.SendPeerVideo([]byte{0x41, 0x33}, 9000, true)
	relay.waitResend(t)

	sent, videoSsrc, _ := relay.snapshot()
	if len(sent) != 2 {
		t.Fatalf("want 2 broadcasts, got %d", len(sent))
	}
	wantSsrc := media.GenerateSecureSsrc(m.currentCall.CallID, m.videoOurDeviceJid, 2)
	if videoSsrc != wantSsrc {
		t.Fatalf("video ssrc = %d want %d", videoSsrc, wantSsrc)
	}

	recvCtx, err := media.NewSrtpContext(peerRecvKM, core.SRTPRecvAuthTagLen)
	if err != nil {
		t.Fatalf("peer recv ctx: %v", err)
	}
	pkt, err := recvCtx.Unprotect(sent[0])
	if err != nil {
		t.Fatalf("peer failed to decrypt our video: %v", err)
	}
	if pkt.Header.PayloadType != core.PayloadTypeWhatsAppH264 {
		t.Fatalf("pt = %d want 97", pkt.Header.PayloadType)
	}
	if pkt.Header.Ssrc != wantSsrc {
		t.Fatalf("ssrc = %d want %d", pkt.Header.Ssrc, wantSsrc)
	}
	if pkt.Header.Marker {
		t.Fatalf("first packet should not carry the marker")
	}
	if string(pkt.Payload) != string([]byte{0x65, 0x11, 0x22}) {
		t.Fatalf("payload mismatch: %x", pkt.Payload)
	}
	seq0 := pkt.Header.SequenceNumber

	pkt1, err := recvCtx.Unprotect(sent[1])
	if err != nil {
		t.Fatalf("decrypt pkt1: %v", err)
	}
	if pkt1.Header.SequenceNumber != seq0+1 {
		t.Fatalf("seq did not increment: %d then %d", seq0, pkt1.Header.SequenceNumber)
	}
	if !pkt1.Header.Marker {
		t.Fatalf("second packet should carry the marker")
	}
}

func TestSendPeerVideoGatedOnVideoCall(t *testing.T) {
	relay := &capRelay{}
	m, _ := newVideoSendCM(t, relay)
	m.localVideo = false
	m.SendPeerVideo([]byte{0x65, 0x11}, 9000, true)
	if sent, _, _ := relay.snapshot(); len(sent) != 0 {
		t.Fatalf("audio-only call must not broadcast video, got %d", len(sent))
	}
}
