package call

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"os"
	"strings"
	"testing"
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
		au := asm.feed(uint16(i), p, marker)
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
	if au := asm.feed(0, []byte{0x61, 0x11, 0x22}, false); au != nil {
		t.Fatalf("no access unit should be emitted without the marker, got %x", au)
	}
}

func loadVideoHexPackets(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
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
