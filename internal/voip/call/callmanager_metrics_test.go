package call

import (
	"log/slog"
	"testing"
	"time"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/engine"
	"wacalls/internal/voip/media"
)

// Streams other than audio are sporadic and carry their own sequence space, so
// feeding them to the receiver stats reads as catastrophic loss and leaks into
// OnQuality and the SSE event.
func TestNonAudioRTPDoesNotFeedQualityStats(t *testing.T) {
	k1, k2 := km(1), km(9)

	recv := NewCallManager(fakeSock{}, slog.Default())
	recv.relay = &fakeRelay{}
	recv.srtp = engine.NewSrtpManager(k2, k1, core.SRTPRecvAuthTagLen, core.SRTPSendAuthTagLen)
	recv.recvStats = media.NewRTCPReceiverStats()
	recv.selfSsrc = 2000
	recv.registerRTPHandler(core.PayloadTypeWhatsAppAppData, func(*media.RtpPacket) {})

	send := NewCallManager(fakeSock{}, slog.Default())
	send.relay = &fakeRelay{onData: recv.onRelayData}
	send.srtp = engine.NewSrtpManager(k1, k2, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)

	// Um buraco enorme entre os sequence numbers: se isso entrasse nas estatisticas
	// do audio, apareceria como perda de ~99,98% no proximo QualitySnapshot.
	for _, seq := range []uint16{1, 9000} {
		pkt := &media.RtpPacket{
			Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppAppData, seq, uint32(seq)*50, 4242),
			Payload: []byte{0x01, 0x02, 0x03},
		}
		if err := send.sendRTP(pkt); err != nil {
			t.Fatalf("sendRTP: %v", err)
		}
	}

	q := recv.recvStats.QualitySnapshot(uint64(time.Now().UnixMilli()))
	if q.LossFraction != 0 {
		t.Fatalf("app-data packets leaked into the audio loss metric: %v", q.LossFraction)
	}
}
