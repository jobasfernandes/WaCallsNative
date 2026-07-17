package call

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"
)

// TestVideoAssemblerEmitsAccessUnitOnMarker replays the real captured WhatsApp keyframe (5 FU-A
// packets) and asserts the assembler emits one Annex-B access unit only on the marker packet.
func TestVideoAssemblerEmitsAccessUnitOnMarker(t *testing.T) {
	pkts := loadVideoHexPackets(t, "../media/h264/testdata/wa_keyframe.txt")
	if len(pkts) != 5 {
		t.Fatalf("fixture must have 5 packets, has %d", len(pkts))
	}

	var asm videoAssembler
	var emitted []byte
	for i, p := range pkts {
		marker := i == len(pkts)-1 // the last fragment closes the frame
		au, _ := asm.feed(uint16(i), p, 3000, marker)
		if i < len(pkts)-1 && au != nil {
			t.Fatalf("packet %d must not emit an access unit before the marker", i)
		}
		if au != nil {
			emitted = au
		}
	}
	if emitted == nil {
		t.Fatal("marker packet must emit an access unit")
	}
	// The reassembled keyframe starts with an Annex-B start code and an SPS (type 7).
	if !bytes.HasPrefix(emitted, []byte{0x00, 0x00, 0x00, 0x01}) {
		t.Fatalf("access unit must start with an Annex-B start code, got %x", emitted[:4])
	}
	if emitted[4]&0x1f != 7 {
		t.Fatalf("first NAL type = %d, want 7 (SPS)", emitted[4]&0x1f)
	}
}

func TestVideoAssemblerNoMarkerNoEmit(t *testing.T) {
	var asm videoAssembler
	// A single complete non-IDR slice without the marker accumulates but does not emit.
	if au, _ := asm.feed(0, []byte{0x61, 0x11, 0x22}, 3000, false); au != nil {
		t.Fatalf("no access unit should be emitted without the marker, got %x", au)
	}
}

func TestVideoAssemblerFrameDurationFromTimestamp(t *testing.T) {
	var asm videoAssembler
	// First frame: no prior timestamp, so the fallback duration is used.
	_, d1 := asm.feed(0, []byte{0x65, 0x11}, 0, true)
	if d1 != 66*time.Millisecond {
		t.Fatalf("first frame duration = %v, want 66ms fallback", d1)
	}
	// Second frame 3000 ticks later at 90 kHz = 33.33ms.
	_, d2 := asm.feed(1, []byte{0x65, 0x22}, 3000, true)
	if d2 != time.Duration(3000)*time.Second/90000 {
		t.Fatalf("second frame duration = %v, want ts-delta/90kHz", d2)
	}
}

func TestParseCVORotation(t *testing.T) {
	// RTP header (12B, X bit set, CC=0) + one-byte-header extension (profile 0xBEDE, len 1 word)
	// carrying a 1-byte CVO element id=3 value with rotation bits = 1 (90 deg).
	pkt := []byte{
		0x90, 0x61, 0x00, 0x01, // V=2,X=1,CC=0 ; PT ; seq
		0x00, 0x00, 0x0b, 0xb8, // timestamp
		0x11, 0x22, 0x33, 0x44, // ssrc
		0xbe, 0xde, 0x00, 0x01, // ext profile 0xBEDE, length = 1 word
		0x30, 0x01, 0x00, 0x00, // id=3 len=1, value=0x01 (rotation 1 -> 90 deg), padding
	}
	if got := parseCVORotation(pkt); got != 90 {
		t.Fatalf("parseCVORotation = %d, want 90", got)
	}
	// No extension (X bit clear) -> -1.
	noExt := []byte{0x80, 0x61, 0, 1, 0, 0, 0, 0, 0x11, 0x22, 0x33, 0x44, 0x65}
	if got := parseCVORotation(noExt); got != -1 {
		t.Fatalf("parseCVORotation(no ext) = %d, want -1", got)
	}
}

func loadVideoHexPackets(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		b, err := hex.DecodeString(parts[len(parts)-1])
		if err != nil {
			t.Fatalf("bad hex: %v", err)
		}
		out = append(out, b)
	}
	return out
}
