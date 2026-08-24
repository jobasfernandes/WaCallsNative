package audio

import (
	"math"
	"testing"

	"wacalls/internal/voip/codec/mlow"
	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
)

func TestAudioDecodesInbound(t *testing.T) {
	codec, err := mlow.NewMLowCodec(mlow.DefaultCodecOptions)
	if err != nil {
		t.Fatalf("NewMLowCodec: %v", err)
	}
	defer codec.Close()

	a := New(codec)
	var got []float32
	a.OnPeerPCM(func(p []float32) { got = p })

	frame := make([]float32, codec.FrameSize())
	for i := range frame {
		frame[i] = 0.3 * float32(math.Sin(2*math.Pi*440*float64(i)/16000))
	}
	enc, err := codec.Encode(frame)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	pkt := &media.RtpPacket{
		Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppOpus, 1, 0, 5000),
		Payload: enc,
	}
	a.handleInbound(pkt)

	if len(got) == 0 {
		t.Fatal("expected decoded peer PCM, got none")
	}
}

func TestAudioConcealsLostPacket(t *testing.T) {
	codec, err := mlow.NewMLowCodec(mlow.DefaultCodecOptions)
	if err != nil {
		t.Fatalf("NewMLowCodec: %v", err)
	}
	defer codec.Close()

	a := New(codec)
	var got [][]float32
	a.OnPeerPCM(func(p []float32) { got = append(got, p) })

	frame := make([]float32, codec.FrameSize())
	for i := range frame {
		frame[i] = 0.3 * float32(math.Sin(2*math.Pi*440*float64(i)/16000))
	}
	enc, err := codec.Encode(frame)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	push := func(seq uint16) {
		a.handleInbound(&media.RtpPacket{
			Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppOpus, seq, 0, 5000),
			Payload: enc,
		})
	}

	push(1) // decoded, becomes lastFrame
	push(3) // seq 2 missing: hold
	push(4) // hold
	push(5) // depth reached: conceal 2, then flush 3,4,5

	// Frames: 1 (real), then concealed-2, 3, 4, 5 = 5 emitted PCM frames.
	if len(got) != 5 {
		t.Fatalf("want 5 played frames (1 real, 1 concealed, 3 real), got %d", len(got))
	}
	// The concealment is a faded copy of the last decoded frame: it starts near the
	// last frame's first sample and fades to zero.
	concealed := got[1]
	if len(concealed) == 0 || concealed[len(concealed)-1] != 0 {
		t.Fatalf("concealment must fade to zero at the tail, got tail %v", concealed[len(concealed)-1])
	}
}

func TestAudioFeedPCMBounds(t *testing.T) {
	codec, err := mlow.NewMLowCodec(mlow.DefaultCodecOptions)
	if err != nil {
		t.Fatalf("NewMLowCodec: %v", err)
	}
	defer codec.Close()

	a := New(codec)
	a.FeedPCM(make([]float32, codec.FrameSize()*10))

	if len(a.captureBuf) > codec.FrameSize()*4 {
		t.Fatalf("captureBuf=%d exceeds bound %d", len(a.captureBuf), codec.FrameSize()*4)
	}
}

// O audio de grupo chega sob o payload type 121 e com um byte a mais na frente
// do TOC. Lido sem descontar esse byte, todo frame se parece com um TOC zerado:
// inativo, e portanto silenciado. A chamada inteira vira silencio.
func TestGroupAudioSkipsTheLeadingPayloadByte(t *testing.T) {
	// TOC 0x50: ativo, 16 kHz, 60 ms.
	const toc = byte(0x50)
	rec := &recordingCodec{}
	a := New(rec)

	a.handleInbound(&media.RtpPacket{
		Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppOpusAlt, 1, 0, 7),
		Payload: []byte{0x00, toc, 0xAA, 0xBB},
	})
	if len(rec.frames) == 0 {
		t.Fatal("o frame nao chegou ao decoder")
	}
	if got := rec.frames[0][0]; got != toc {
		t.Errorf("primeiro byte entregue ao codec = %#02x, want o TOC %#02x", got, toc)
	}

	// Numa chamada 1:1 o payload comeca no proprio TOC: nada a descontar.
	rec.frames = nil
	a.handleInbound(&media.RtpPacket{
		Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppOpus, 2, 960, 7),
		Payload: []byte{toc, 0xAA, 0xBB},
	})
	if len(rec.frames) == 0 {
		t.Fatal("o frame 1:1 nao chegou ao decoder")
	}
	if got := rec.frames[0][0]; got != toc {
		t.Errorf("1:1: primeiro byte = %#02x, want o TOC %#02x", got, toc)
	}
}

type recordingCodec struct{ frames [][]byte }

func (c *recordingCodec) Decode(f []byte) ([]float32, error) {
	c.frames = append(c.frames, append([]byte(nil), f...))
	return make([]float32, 960), nil
}
func (c *recordingCodec) Encode([]float32) ([]byte, error) { return nil, nil }
func (c *recordingCodec) FrameSize() int                   { return 960 }
func (c *recordingCodec) SampleRate() int                  { return 16000 }
func (c *recordingCodec) Close()                           {}
