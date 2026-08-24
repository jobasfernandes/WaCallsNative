package transport

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// Vetores de uma chamada de grupo capturada: o relay multi-participante prefixa
// um header de forwarding cujo tamanho depende do subtipo em data[1].
func TestUnwrapGroupForwardingPacketCaptureVectors(t *testing.T) {
	vectors := []struct {
		name      string
		packetHex string
		innerHex  string
	}{
		{
			name:      "subtype 2, video",
			packetHex: "0902336e0100018890e1001300120c96772e6aa9",
			innerHex:  "90e1001300120c96772e6aa9",
		},
		{
			name:      "subtype 4, video",
			packetHex: "09047eb5410001000000001090e100010013fbf08f1df407",
			innerHex:  "90e100010013fbf08f1df407",
		},
		{
			name:      "subtype 7, audio",
			packetHex: "0907338f2900020a00c801e000000000000090780001000331808cd481d5",
			innerHex:  "90780001000331808cd481d5",
		},
	}
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			packet, err := hex.DecodeString(v.packetHex)
			if err != nil {
				t.Fatalf("decode packet: %v", err)
			}
			want, err := hex.DecodeString(v.innerHex)
			if err != nil {
				t.Fatalf("decode inner: %v", err)
			}
			got, wrapped, valid := UnwrapGroupForwardingPacket(packet)
			if !wrapped {
				t.Fatal("captured packet was not recognized as forwarded")
			}
			if !valid {
				t.Fatal("captured packet was rejected")
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("inner = %x, want %x", got, want)
			}
		})
	}
}

// Um pacote sem o marcador passa intacto: o caminho 1:1 nao e encapsulado.
func TestUnwrapGroupForwardingPacketPassesPlainRTP(t *testing.T) {
	plain := []byte{0x90, 0x78, 0x00, 0x01, 0x00, 0x03, 0x31, 0x80, 0x8c, 0xd4, 0x81, 0xd5}
	got, wrapped, valid := UnwrapGroupForwardingPacket(plain)
	if wrapped {
		t.Error("a plain RTP packet must not be reported as forwarded")
	}
	if !valid {
		t.Error("a plain RTP packet must be valid")
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("got %x, want the input unchanged", got)
	}
}

func TestUnwrapGroupForwardingPacketRejectsMalformed(t *testing.T) {
	cases := []struct {
		name   string
		packet []byte
	}{
		{"marker only", []byte{0x09}},
		{"shorter than its own header", []byte{0x09, 0x03, 0x00, 0x00}},
		{"one byte short of the header", []byte{0x09, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00}},
		{"header plus a single byte", []byte{0x09, 0x02, 0, 0, 0, 0, 0, 0, 0x80}},
		{"inner is not RTP version 2", append([]byte{0x09, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			[]byte{0x00, 0x78, 0x00, 0x01, 0x00, 0x03, 0x31, 0x80, 0x8c, 0xd4, 0x81, 0xd5}...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, wrapped, valid := UnwrapGroupForwardingPacket(tc.packet)
			if !wrapped {
				t.Fatal("a packet carrying the marker must be reported as forwarded")
			}
			if valid {
				t.Error("expected the packet to be rejected")
			}
		})
	}
}

// Entrada vazia nao pode entrar em panico nem ser tratada como encapsulada.
func TestUnwrapGroupForwardingPacketEmpty(t *testing.T) {
	got, wrapped, valid := UnwrapGroupForwardingPacket(nil)
	if wrapped || !valid || got != nil {
		t.Fatalf("nil input: got %v, wrapped=%v, valid=%v; want nil, false, true", got, wrapped, valid)
	}
}

// O tamanho do header cresce com o subtipo, e a captura confirma a progressao em
// todos os pacotes que o relay entrega. Uma tabela com tres entradas fixas
// rejeita os demais subtipos, e com eles vai o RTCP: sem relatorio de recepcao a
// chamada nunca reporta latencia nem perda.
func TestForwardingHeaderLengthFollowsTheSubtype(t *testing.T) {
	for _, tc := range []struct {
		subtype byte
		header  int
	}{{2, 8}, {3, 10}, {4, 12}, {5, 14}, {6, 16}, {7, 18}, {8, 20}} {
		payload := make([]byte, 16)
		payload[0] = 0x80 // RTP version 2
		packet := append(append([]byte{0x09, tc.subtype}, make([]byte, tc.header-2)...), payload...)
		got, wrapped, valid := UnwrapGroupForwardingPacket(packet)
		if !wrapped || !valid {
			t.Errorf("subtype %#02x: wrapped=%v valid=%v, want a %d byte header",
				tc.subtype, wrapped, valid, tc.header)
			continue
		}
		if len(got) != len(payload) {
			t.Errorf("subtype %#02x: payload = %d bytes, want %d",
				tc.subtype, len(got), len(payload))
		}
	}
}

// Alguns subtipos chegam so com o header e nenhum conteudo. Nao sao midia nem
// erro: tratar como malformado enche o log de avisos por trafego normal.
func TestHeaderOnlyForwardingPacketIsNotAnError(t *testing.T) {
	for _, subtype := range []byte{3, 5, 6} {
		header := 2*int(subtype) + 4
		packet := append([]byte{0x09, subtype}, make([]byte, header-2)...)
		payload, wrapped, valid := UnwrapGroupForwardingPacket(packet)
		if !wrapped || !valid {
			t.Errorf("subtype %#02x: header-only packet reported invalid", subtype)
		}
		if len(payload) != 0 {
			t.Errorf("subtype %#02x: payload = %d bytes, want none", subtype, len(payload))
		}
	}
}
