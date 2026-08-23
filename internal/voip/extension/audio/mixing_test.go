package audio

import (
	"testing"

	"wacalls/internal/voip/codec/mlow"
	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
)

func newDecoder() (core.AudioCodec, error) {
	return mlow.NewMLowCodec(mlow.DefaultCodecOptions)
}

// toneFrame gera um frame de amplitude constante, para a soma ser previsivel.
func toneFrame(t *testing.T, codec core.AudioCodec, amplitude float32) []byte {
	t.Helper()
	pcm := make([]float32, codec.FrameSize())
	for i := range pcm {
		pcm[i] = amplitude
	}
	enc, err := codec.Encode(pcm)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return enc
}

func pkt(ssrc uint32, seq uint16, payload []byte) *media.RtpPacket {
	return &media.RtpPacket{
		Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppOpus, seq, uint32(seq)*960, ssrc),
		Payload: payload,
	}
}

func groupAudio(t *testing.T) (*Audio, core.AudioCodec) {
	t.Helper()
	codec, err := newDecoder()
	if err != nil {
		t.Fatalf("codec: %v", err)
	}
	a := New(codec)
	a.SetDecoderFactory(newDecoder)
	return a, codec
}

// Dois participantes entregando o mesmo frame produzem um frame somado.
func TestMixSumsTwoParticipants(t *testing.T) {
	a, codec := groupAudio(t)
	defer a.Detach()

	var got [][]float32
	a.OnPeerPCM(func(pcm []float32) { got = append(got, append([]float32(nil), pcm...)) })

	frame := toneFrame(t, codec, 0.2)
	// O primeiro pacote de um participante e o que revela que ele existe, entao
	// esse frame sai sozinho: o barrier so pode esperar por quem ele ja conhece.
	a.handleInbound(pkt(10, 1, frame))
	if len(got) != 1 {
		t.Fatalf("the first frame of the first participant must go out, got %d", len(got))
	}
	// Agora o barrier vale: o segundo participante espera o primeiro.
	a.handleInbound(pkt(20, 1, frame))
	if len(got) != 1 {
		t.Fatalf("emitted %d, want the mix to wait for the other participant", len(got))
	}
	a.handleInbound(pkt(10, 2, frame))
	if len(got) != 2 {
		t.Fatalf("emitted %d, want a mixed frame once both had one ready", len(got))
	}
	got = got[1:] // o frame mixado

	// A soma tem de ser maior que um stream sozinho, e nao pode ser identica a ele.
	solo, _ := codec.Decode(frame)
	var sumEnergy, soloEnergy float64
	for i := range got[0] {
		sumEnergy += float64(got[0][i]) * float64(got[0][i])
		soloEnergy += float64(solo[i]) * float64(solo[i])
	}
	if sumEnergy <= soloEnergy {
		t.Errorf("mixed energy %v must exceed a single stream's %v", sumEnergy, soloEnergy)
	}
}

// A soma satura em vez de estourar a faixa.
func TestMixClipsInsteadOfOverflowing(t *testing.T) {
	a, codec := groupAudio(t)
	defer a.Detach()

	var got []float32
	a.OnPeerPCM(func(pcm []float32) { got = append([]float32(nil), pcm...) })

	loud := toneFrame(t, codec, 0.95)
	a.handleInbound(pkt(10, 1, loud)) // estabelece o stream 10
	a.handleInbound(pkt(20, 1, loud)) // estabelece o 20, segura
	a.handleInbound(pkt(10, 2, loud)) // agora mixa os dois

	if len(got) == 0 {
		t.Fatal("expected a mixed frame")
	}
	for i, v := range got {
		if v > 1.0 || v < -1.0 {
			t.Fatalf("sample %d = %v, outside [-1, 1]", i, v)
		}
	}
}

// Um participante que para de entregar nao pode travar o audio dos outros.
func TestMixReleasesWhenOneParticipantLags(t *testing.T) {
	a, codec := groupAudio(t)
	defer a.Detach()

	var emitted int
	a.OnPeerPCM(func([]float32) { emitted++ })

	frame := toneFrame(t, codec, 0.2)
	// O primeiro pacote de cada um estabelece os dois streams.
	a.handleInbound(pkt(10, 1, frame))
	a.handleInbound(pkt(20, 1, frame))
	emitted = 0

	// Agora so um entrega. Ate maxLag o barrier segura.
	for seq := uint16(2); seq <= uint16(1+maxMixLag); seq++ {
		a.handleInbound(pkt(10, seq, frame))
	}
	if emitted == 0 {
		t.Fatal("a participant that stopped delivering must not hold the others forever")
	}
}

// O atrasado volta a ser mixado quando entrega de novo.
func TestMixResumesLaggingParticipant(t *testing.T) {
	a, codec := groupAudio(t)
	defer a.Detach()

	var emitted int
	a.OnPeerPCM(func([]float32) { emitted++ })

	frame := toneFrame(t, codec, 0.2)
	a.handleInbound(pkt(10, 1, frame))
	a.handleInbound(pkt(20, 1, frame))
	for seq := uint16(2); seq <= uint16(1+maxMixLag); seq++ {
		a.handleInbound(pkt(10, seq, frame))
	}
	before := emitted

	a.handleInbound(pkt(20, 2, frame))
	if emitted <= before {
		t.Error("a participant that resumed delivering must be mixed again")
	}
}

// Cada participante precisa do seu decoder: o MLow carrega estado entre frames, e
// um decoder compartilhado entre streams produz lixo.
func TestEachParticipantGetsItsOwnDecoder(t *testing.T) {
	a, _ := groupAudio(t)
	defer a.Detach()

	a.handleInbound(pkt(10, 1, []byte{0x50, 0x00, 0x00}))
	a.handleInbound(pkt(20, 1, []byte{0x50, 0x00, 0x00}))

	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.streams) != 2 {
		t.Fatalf("streams = %d, want one per SSRC", len(a.streams))
	}
	if a.streams[10].codec == a.streams[20].codec {
		t.Error("two participants must not share a decoder: MLow carries state between frames")
	}
}

// Sem factory, um SSRC novo e descartado em vez de entrar em panico. Isso so
// acontece se um participante de grupo chegar numa chamada que nao e de grupo.
func TestSecondSSRCWithoutFactoryIsDropped(t *testing.T) {
	codec, err := newDecoder()
	if err != nil {
		t.Fatalf("codec: %v", err)
	}
	a := New(codec)
	defer a.Detach()

	a.handleInbound(pkt(10, 1, []byte{0x50, 0x00, 0x00}))
	a.handleInbound(pkt(20, 1, []byte{0x50, 0x00, 0x00}))

	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.streams) != 1 {
		t.Fatalf("streams = %d, want only the first without a decoder factory", len(a.streams))
	}
}
