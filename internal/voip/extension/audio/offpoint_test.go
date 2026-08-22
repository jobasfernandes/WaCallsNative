package audio

import (
	"testing"

	"wacalls/internal/voip/codec/mlow"
	"wacalls/internal/voip/codec/nativemlow"
	"wacalls/internal/voip/codec/opus"
	"wacalls/internal/voip/core"
)

// A contagem so serve se sobreviver a cadeia de wrappers que producao monta:
// mlow -> nativemlow.WrapEncoder -> opus.WithFallback.
func TestOffPointCountsSurviveTheProductionCodecChain(t *testing.T) {
	base, err := mlow.NewMLowCodec(mlow.DefaultCodecOptions)
	if err != nil {
		t.Fatalf("NewMLowCodec: %v", err)
	}
	wrapped, _ := nativemlow.WrapEncoder(base)
	codec := opus.WithFallback(wrapped)

	counter, ok := codec.(core.OffPointCounter)
	if !ok {
		t.Fatal("the production codec chain must still report off-point counts")
	}
	if counts := counter.OffPointCounts(); counts != nil {
		t.Fatalf("a fresh codec must report nothing, got %v", counts)
	}

	// 0x90 e um frame SID real da captura: inativo, silenciado, esperado.
	if _, err := codec.Decode([]byte{0x90, 0x00, 0x00}); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	// 0x54 e o operating point low-rate que este decoder nao implementa.
	if _, err := codec.Decode([]byte{0x54, 0x00, 0x00}); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	counts := counter.OffPointCounts()
	if counts[mlow.OffPointInactive] != 1 {
		t.Errorf("inactive = %d, want 1", counts[mlow.OffPointInactive])
	}
	if counts[mlow.OffPointLowRate] != 1 {
		t.Errorf("low_rate = %d, want 1", counts[mlow.OffPointLowRate])
	}
}

// Standard-Opus frames sao roteados pelo fallback antes da guarda do MLow, entao
// nao podem ser contados como off point.
func TestStandardOpusFramesAreNotCountedAsOffPoint(t *testing.T) {
	base, err := mlow.NewMLowCodec(mlow.DefaultCodecOptions)
	if err != nil {
		t.Fatalf("NewMLowCodec: %v", err)
	}
	codec := opus.WithFallback(base)
	if _, err := codec.Decode([]byte{0xC0, 0x00, 0x00}); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	counter, ok := codec.(core.OffPointCounter)
	if !ok {
		t.Skip("fallback decoder unavailable in this build")
	}
	if n := counter.OffPointCounts()[mlow.OffPointStdOpus]; n != 0 {
		t.Errorf("std_opus counted %d times; the fallback handles those before the guard", n)
	}
}

// Detach nao pode entrar em panico com um codec que nao reporta contagem.
func TestDetachToleratesCodecWithoutCounts(t *testing.T) {
	codec, err := mlow.NewMLowCodec(mlow.DefaultCodecOptions)
	if err != nil {
		t.Fatalf("NewMLowCodec: %v", err)
	}
	a := New(codec)
	a.Detach()
}
