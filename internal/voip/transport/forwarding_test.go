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
		{"unknown subtype", []byte{0x09, 0x03, 0x00, 0x00}},
		{"truncated before inner RTP", []byte{0x09, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
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
