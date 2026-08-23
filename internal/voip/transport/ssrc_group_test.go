package transport

import (
	"bytes"
	"encoding/binary"
	"testing"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
)

// Os seis primeiros slots sao derivados deterministicamente dos dois lados: e
// assim que os participantes descobrem os SSRCs uns dos outros sem SDP.
func TestDeriveRelayStreamSSRCsIsDeterministic(t *testing.T) {
	a := DeriveRelayStreamSSRCs("CALL1", "peer:0@lid")
	b := DeriveRelayStreamSSRCs("CALL1", "peer:0@lid")
	if a != b {
		t.Fatal("derivation must be deterministic")
	}
	// O slot de audio (counter 0) tem de bater com a derivacao 1:1 que ja existe.
	want := media.GenerateSecureSsrc("CALL1", "peer:0@lid", core.SsrcCounterAudio)
	if a[0] != want {
		t.Errorf("slot 0 = %08x, want the 1:1 audio SSRC %08x", a[0], want)
	}
}

// O indice 8 do plano carrega o slot word 6, que e tambem o do app-data: derivar
// os nove de forma ingenua anuncia um mesmo SSRC em dois grupos de subscription.
func TestDeriveRelayStreamSSRCsCollidesWithAppDataByDesign(t *testing.T) {
	derived := DeriveRelayStreamSSRCs("CALL1", "our:0@lid")
	appData := media.GenerateSecureSsrc("CALL1", "our:0@lid", core.SsrcCounterAppData)
	if derived[8] != appData {
		t.Fatalf("slot index 8 = %08x, expected it to equal the app-data SSRC %08x; "+
			"if this changed, PrepareRelayStreamSSRCs may no longer be needed", derived[8], appData)
	}
}

// O nucleo desta task: os tres slots auxiliares nao podem colidir com nada.
func TestPrepareRelayStreamSSRCsAvoidsCollisions(t *testing.T) {
	base := [9]uint32{10, 11, 12, 13, 14, 15, 99, 99, 99}
	const appData = uint32(777)
	got, err := PrepareRelayStreamSSRCs(base, appData, bytes.NewReader(encodeLE(1001, 1002, 1003)))
	if err != nil {
		t.Fatalf("PrepareRelayStreamSSRCs: %v", err)
	}
	if [6]uint32(got[:6]) != [6]uint32(base[:6]) {
		t.Errorf("the first six slots must be preserved: got %v, want %v", got[:6], base[:6])
	}
	seen := map[uint32]string{appData: "app-data"}
	for _, s := range base[:6] {
		seen[s] = "base slot"
	}
	for i := 6; i < 9; i++ {
		if got[i] == 0 {
			t.Errorf("slot %d is zero", i)
		}
		if who, clash := seen[got[i]]; clash {
			t.Errorf("slot %d = %08x collides with %s", i, got[i], who)
		}
		seen[got[i]] = "auxiliary slot"
	}
}

// O gerador tem de descartar zero e valores ja usados e tentar de novo.
func TestPrepareRelayStreamSSRCsRetriesOnZeroAndDuplicate(t *testing.T) {
	base := [9]uint32{10, 11, 12, 13, 14, 15, 0, 0, 0}
	// Sequencia: 0 (descarta), 10 (ja usado, descarta), 500, 501, 502.
	feed := encodeLE(0, 10, 500, 501, 502)
	got, err := PrepareRelayStreamSSRCs(base, 777, bytes.NewReader(feed))
	if err != nil {
		t.Fatalf("PrepareRelayStreamSSRCs: %v", err)
	}
	for i, want := range map[int]uint32{6: 500, 7: 501, 8: 502} {
		if got[i] != want {
			t.Errorf("slot %d = %d, want %d", i, got[i], want)
		}
	}
}

func TestPrepareRelayStreamSSRCsFailsWithoutRandom(t *testing.T) {
	if _, err := PrepareRelayStreamSSRCs([9]uint32{}, 0, nil); err == nil {
		t.Error("a nil random source must be an error, not a silent zero SSRC")
	}
}

// Uma fonte que acaba no meio nao pode devolver SSRC zerado em silencio.
func TestPrepareRelayStreamSSRCsFailsOnExhaustedRandom(t *testing.T) {
	if _, err := PrepareRelayStreamSSRCs([9]uint32{}, 0, bytes.NewReader(encodeLE(1))); err == nil {
		t.Error("an exhausted random source must be an error")
	}
}

func encodeLE(values ...uint32) []byte {
	out := make([]byte, 0, len(values)*4)
	for _, v := range values {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		out = append(out, b[:]...)
	}
	return out
}
