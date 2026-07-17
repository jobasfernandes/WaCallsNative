package h264

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func TestSingleNalUnitIsReturnedUnchanged(t *testing.T) {
	// NAL header 0x61 = F=0, NRI=3, type=1 (non-IDR slice): a single NAL unit packet.
	payload := []byte{0x61, 0xe0, 0x00, 0x5c, 0x88, 0x8f}

	var d Depacketizer
	nals, err := d.Depacketize(0, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nals) != 1 {
		t.Fatalf("want 1 NAL, got %d", len(nals))
	}
	if !bytes.Equal(nals[0], payload) {
		t.Fatalf("single NAL must be the payload unchanged\n got=%x\nwant=%x", nals[0], payload)
	}
}

func TestFuAReassemblesFragmentedNal(t *testing.T) {
	start := []byte{0x7c, 0x85, 0xaa, 0xbb} // FU ind type 28 NRI 3, FU hdr S=1 type 5
	end := []byte{0x7c, 0x45, 0xcc, 0xdd}   // FU hdr E=1 type 5
	want := []byte{0x65, 0xaa, 0xbb, 0xcc, 0xdd}

	var d Depacketizer
	nals, err := d.Depacketize(0, start)
	if err != nil {
		t.Fatalf("start error: %v", err)
	}
	if len(nals) != 0 {
		t.Fatalf("start must not emit a NAL yet, got %d", len(nals))
	}
	nals, err = d.Depacketize(1, end)
	if err != nil {
		t.Fatalf("end error: %v", err)
	}
	if len(nals) != 1 {
		t.Fatalf("want 1 reassembled NAL, got %d", len(nals))
	}
	if !bytes.Equal(nals[0], want) {
		t.Fatalf("reassembled NAL wrong\n got=%x\nwant=%x", nals[0], want)
	}
}

func TestSeqGapDropsPartialAndWaitsKeyframe(t *testing.T) {
	var d Depacketizer
	// Start a FU-A, then a sequence gap: the partial NAL is dropped and non-keyframe NALs
	// are discarded until an SPS/IDR arrives.
	if _, err := d.Depacketize(0, []byte{0x7c, 0x85, 0xaa}); err != nil {
		t.Fatal(err)
	}
	nals, err := d.Depacketize(5, []byte{0x61, 0x00}) // gap (0 -> 5), non-IDR slice
	if err != nil {
		t.Fatal(err)
	}
	if len(nals) != 0 {
		t.Fatalf("non-keyframe after a gap must be gated, got %d", len(nals))
	}
	// A single-NAL IDR (type 5) re-opens the gate and is emitted.
	nals, err = d.Depacketize(6, []byte{0x65, 0x11})
	if err != nil {
		t.Fatal(err)
	}
	if len(nals) != 1 || nals[0][0]&0x1f != 5 {
		t.Fatalf("IDR must re-open the keyframe gate, got %d nals", len(nals))
	}
}

func TestAnnexBPrefixesEachNalWithStartCode(t *testing.T) {
	nals := [][]byte{{0x67, 0x4d}, {0x68, 0xee}, {0x65, 0xb8}}
	want := []byte{
		0x00, 0x00, 0x00, 0x01, 0x67, 0x4d,
		0x00, 0x00, 0x00, 0x01, 0x68, 0xee,
		0x00, 0x00, 0x00, 0x01, 0x65, 0xb8,
	}
	if got := AnnexB(nals); !bytes.Equal(got, want) {
		t.Fatalf("Annex-B wrong\n got=%x\nwant=%x", got, want)
	}
}

// TestRealWhatsAppKeyframeReassemblesToValidH264 replays a real captured WhatsApp video
// keyframe (5 FU-A packets) and asserts it reassembles into an access unit whose Annex-B
// carries SPS(7), PPS(8) and IDR(5) in order.
func TestRealWhatsAppKeyframeReassemblesToValidH264(t *testing.T) {
	pkts := loadHexPackets(t, "testdata/wa_keyframe.txt")
	if len(pkts) != 5 {
		t.Fatalf("fixture must have 5 packets, has %d", len(pkts))
	}

	var d Depacketizer
	var got [][]byte
	for i, p := range pkts {
		nals, err := d.Depacketize(uint16(i), p)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if i < 4 && len(nals) != 0 {
			t.Fatalf("packet %d (mid fragment) must not emit a NAL, emitted %d", i, len(nals))
		}
		got = append(got, nals...)
	}
	if len(got) != 1 {
		t.Fatalf("keyframe must reassemble into 1 unit, got %d", len(got))
	}

	types := nalTypesInAnnexB(AnnexB(got))
	want := []byte{7, 8, 5}
	if !bytes.Equal(types, want) {
		t.Fatalf("keyframe NAL types\n got=%v\nwant=%v (SPS,PPS,IDR)", types, want)
	}
}

func TestIsPlausibleNALHeader(t *testing.T) {
	cases := []struct {
		b    byte
		want bool
	}{
		{0x61, true}, {0x65, true}, {0x67, true}, {0x68, true},
		{0x7c, true}, {0x1c, true},
		{0x80, false}, {0x00, false}, {0x10, false},
	}
	for _, c := range cases {
		if got := IsPlausibleNALHeader(c.b); got != c.want {
			t.Errorf("IsPlausibleNALHeader(0x%02x)=%v, want %v", c.b, got, c.want)
		}
	}
}

func loadHexPackets(t *testing.T, path string) [][]byte {
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

func nalTypesInAnnexB(b []byte) []byte {
	var types []byte
	i := 0
	for i < len(b)-3 {
		if b[i] == 0 && b[i+1] == 0 {
			if b[i+2] == 1 {
				types = append(types, b[i+3]&0x1f)
				i += 4
				continue
			}
			if i < len(b)-4 && b[i+2] == 0 && b[i+3] == 1 {
				types = append(types, b[i+4]&0x1f)
				i += 5
				continue
			}
		}
		i++
	}
	return types
}
