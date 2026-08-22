package mlow

import "testing"

// Os TOCs vem de testdata/inbound_capture_frames.json, uma captura de chamada
// real, mais o caso low-rate construido do mesmo jeito que o vetor sintetico da
// implementacao de referencia (bit 2 setado sobre um frame ativo).
func TestOffOperatingPointReason(t *testing.T) {
	cases := []struct {
		name string
		toc  byte
		want string
	}{
		{"active 60ms 16k decodes", 0x50, ""},
		{"active 60ms 16k with bit1 decodes", 0x12, ""},
		{"sid comfort noise", 0x90, OffPointInactive},
		{"inactive 10ms", 0x00, OffPointInactive},
		{"low rate two subframes", 0x54, OffPointLowRate},
		{"standard opus", 0xC0, OffPointStdOpus},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OffOperatingPointReason(ParseSmplTOC(tc.toc)); got != tc.want {
				t.Errorf("OffOperatingPointReason(0x%02x) = %q, want %q", tc.toc, got, tc.want)
			}
		})
	}
}

// A funcao extraida tem de decidir exatamente o que a guarda do decoder decide:
// se divergirem, o contador mente em silencio.
func TestOffOperatingPointMatchesDecoderGuard(t *testing.T) {
	d := NewMlowDecoder()
	for toc := 0; toc <= 0xff; toc++ {
		b := byte(toc)
		parsed := ParseSmplTOC(b)
		offPoint := OffOperatingPointReason(parsed) != ""
		// Um frame de um byte so: se o decoder for decodar de verdade ele vai
		// falhar ou devolver algo, mas o que interessa aqui e o tamanho da saida
		// do ramo de silencio, que e derivado de FrameMs.
		if !offPoint {
			continue
		}
		out := d.decodeFrame([]byte{b})
		wantLen := 16000 / 1000 * parsed.FrameMs
		if wantLen <= 0 {
			wantLen = opusFrameSamps
		}
		if len(out) != wantLen {
			t.Fatalf("toc 0x%02x: guard emitted %d samples, want %d", b, len(out), wantLen)
		}
	}
}
