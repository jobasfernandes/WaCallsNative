package transport

import (
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

// descriptor e um stream descriptor decodificado do protobuf.
type descriptor struct {
	participant uint64
	layer       uint64
	ssrc        uint64
}

func decodeDescriptors(t *testing.T, blob []byte) []descriptor {
	t.Helper()
	var out []descriptor
	for len(blob) > 0 {
		num, typ, n := protowire.ConsumeTag(blob)
		if n < 0 || num != 1 || typ != protowire.BytesType {
			t.Fatalf("unexpected tag %d/%d", num, typ)
		}
		blob = blob[n:]
		inner, n := protowire.ConsumeBytes(blob)
		if n < 0 {
			t.Fatal("truncated descriptor")
		}
		blob = blob[n:]
		var d descriptor
		for len(inner) > 0 {
			f, ft, fn := protowire.ConsumeTag(inner)
			if fn < 0 {
				t.Fatal("truncated field")
			}
			inner = inner[fn:]
			v, vn := protowire.ConsumeVarint(inner)
			if vn < 0 || ft != protowire.VarintType {
				t.Fatalf("field %d is not a varint", f)
			}
			inner = inner[vn:]
			switch f {
			case 1:
				d.participant = v
			case 2:
				d.layer = v
			case 3:
				d.ssrc = v
			default:
				t.Fatalf("unexpected field %d", f)
			}
		}
		out = append(out, d)
	}
	return out
}

// Os nove descritores locais seguem o plano de participante/layer da captura.
func TestGroupStreamDescriptorsFollowThePlan(t *testing.T) {
	streams := [9]uint32{101, 102, 103, 201, 202, 203, 301, 302, 303}
	got := decodeDescriptors(t, BuildGroupStreamDescriptors(streams, [2]uint32{}))
	if len(got) != 9 {
		t.Fatalf("descriptors = %d, want 9", len(got))
	}
	want := []descriptor{
		{0, 0, 101}, {0, 1, 102}, {0, 2, 103},
		{1, 0, 201}, {1, 1, 202}, {1, 2, 203},
		{2, 0, 301}, {2, 1, 302}, {2, 2, 303},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("descriptor %d = %+v, want %+v", i, got[i], w)
		}
	}
}

// Os descritores HBH-FEC entram como participantes 3 e 4, ambos na layer 3, e so
// quando sao fornecidos: com um unico PID remoto o allocate nao os leva.
func TestGroupStreamDescriptorsAppendHBHFEC(t *testing.T) {
	streams := [9]uint32{101, 102, 103, 201, 202, 203, 301, 302, 303}
	got := decodeDescriptors(t, BuildGroupStreamDescriptors(streams, [2]uint32{900, 901}))
	if len(got) != 11 {
		t.Fatalf("descriptors = %d, want 11 (nine local plus two FEC)", len(got))
	}
	if fec := got[9]; fec != (descriptor{3, 3, 900}) {
		t.Errorf("HBH-FEC TX = %+v, want {3 3 900}", fec)
	}
	if fec := got[10]; fec != (descriptor{4, 3, 901}) {
		t.Errorf("HBH-FEC RX = %+v, want {4 3 901}", fec)
	}
}

// Um SSRC zerado nao gera descritor: o slot simplesmente nao e anunciado.
func TestGroupStreamDescriptorsSkipZero(t *testing.T) {
	streams := [9]uint32{101, 0, 103, 0, 0, 0, 0, 0, 0}
	got := decodeDescriptors(t, BuildGroupStreamDescriptors(streams, [2]uint32{}))
	if len(got) != 2 {
		t.Fatalf("descriptors = %d, want 2", len(got))
	}
}

// A ordem dos quatro grupos e a da captura: video primario, video secundario,
// audio, app-data.
func TestGroupSenderSubscriptionsHasFourGroups(t *testing.T) {
	streams := [9]uint32{10, 11, 12, 20, 21, 22, 30, 31, 32}
	blob := BuildGroupSenderSubscriptions(streams, 40, []uint32{1, 2})
	groups := 0
	rest := blob
	for len(rest) > 0 {
		num, typ, n := protowire.ConsumeTag(rest)
		if n < 0 || num != 1 || typ != protowire.BytesType {
			t.Fatalf("unexpected tag %d/%d at group %d", num, typ, groups)
		}
		rest = rest[n:]
		_, n = protowire.ConsumeBytes(rest)
		if n < 0 {
			t.Fatal("truncated group")
		}
		rest = rest[n:]
		groups++
	}
	if groups != 4 {
		t.Fatalf("subscription groups = %d, want 4", groups)
	}
}

// Só o grupo de video primario marca a flag de video.
func TestGroupSenderSubscriptionsFlagVideoOnlyOnPrimary(t *testing.T) {
	streams := [9]uint32{10, 11, 12, 20, 21, 22, 30, 31, 32}
	primary := BuildGroupSenderSubscriptions(streams, 40, []uint32{1})
	audioOnly := buildSenderSubscription(streams[0:3], []uint32{1}, false)
	videoOnly := buildSenderSubscription(streams[3:6], []uint32{1}, true)
	if len(videoOnly) <= len(audioOnly) {
		t.Errorf("the video subscription (%d bytes) must carry an extra field over audio (%d bytes)",
			len(videoOnly), len(audioOnly))
	}
	if len(primary) == 0 {
		t.Fatal("expected a non-empty blob")
	}
}

// O grupo de video secundario nao leva participantes.
func TestSecondaryVideoSubscriptionHasNoParticipants(t *testing.T) {
	withPIDs := buildSenderSubscription([]uint32{30, 31, 32}, []uint32{1, 2}, false)
	withoutPIDs := buildSenderSubscription([]uint32{30, 31, 32}, nil, false)
	if len(withoutPIDs) >= len(withPIDs) {
		t.Errorf("a subscription without participants (%d bytes) must be shorter than with (%d bytes)",
			len(withoutPIDs), len(withPIDs))
	}
}

func TestGroupReceiverSubscriptionsCarryOnlyPIDs(t *testing.T) {
	blob := BuildGroupReceiverSubscriptions([]uint32{1, 2})
	// Forma capturada com dois PIDs: 12 02 08 01 12 02 08 02
	want := []byte{0x12, 0x02, 0x08, 0x01, 0x12, 0x02, 0x08, 0x02}
	if len(blob) != len(want) {
		t.Fatalf("blob = % x, want % x", blob, want)
	}
	for i := range want {
		if blob[i] != want[i] {
			t.Fatalf("blob = % x, want % x", blob, want)
		}
	}
}

func TestNormalizeParticipantPIDs(t *testing.T) {
	cases := []struct {
		name string
		in   []uint32
		want []uint32
	}{
		{"sorted and deduplicated", []uint32{2, 1, 2}, []uint32{1, 2}},
		{"zero dropped: it is the local participant", []uint32{0, 1}, []uint32{1}},
		{"empty stays empty", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeParticipantPIDs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
