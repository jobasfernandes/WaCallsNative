package media

import (
	"encoding/binary"
	"testing"
)

// O SR de grupo difere do 1:1 em tres pontos observados em captura: leva
// exatamente um bloco de recepcao, carrega uma extensao opaca de 8 bytes ao
// final, e nao leva SDES.
func TestBuildGroupSenderReportShape(t *testing.T) {
	const selfSsrc, peerSsrc = uint32(0x11223344), uint32(0x55667788)
	ext := RTCPGroupReportExtension{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	rb := &RTCPReportBlock{SSRC: peerSsrc, FractionLost: 3, ExtHighSeq: 1200}
	stats := RTCPSenderStats{PacketsSent: 100, OctetsSent: 16000, RtpTimestamp: 9000}

	packet := BuildGroupSenderReport(selfSsrc, stats, rb, ext, 1_700_000_000_000)

	if v := packet[0] >> 6; v != 2 {
		t.Errorf("RTCP version = %d, want 2", v)
	}
	if rc := packet[0] & 0x1f; rc != 1 {
		t.Errorf("report count = %d, want 1", rc)
	}
	if pt := packet[1]; pt != RTCPPayloadTypeSR {
		t.Errorf("payload type = %d, want %d (SR)", pt, RTCPPayloadTypeSR)
	}
	if got := binary.BigEndian.Uint32(packet[4:8]); got != selfSsrc {
		t.Errorf("sender SSRC = %08x, want %08x", got, selfSsrc)
	}
	if got := binary.BigEndian.Uint32(packet[28:32]); got != peerSsrc {
		t.Errorf("report block SSRC = %08x, want %08x", got, peerSsrc)
	}
	// O length e em words de 32 bits menos um, e tem de cobrir a extensao.
	if got, want := binary.BigEndian.Uint16(packet[2:4]), uint16(len(packet)/4-1); got != want {
		t.Errorf("length field = %d, want %d", got, want)
	}
	if len(packet)%4 != 0 {
		t.Errorf("packet length %d is not a multiple of 4", len(packet))
	}
	// A extensao vai nos ultimos oito bytes, intacta.
	tail := packet[len(packet)-len(ext):]
	for i, b := range ext {
		if tail[i] != b {
			t.Fatalf("extension = %x, want %x", tail, ext)
		}
	}
}

// SDES no SR de grupo faz o servidor rejeitar: a captura nao tem.
func TestBuildGroupSenderReportHasNoSDES(t *testing.T) {
	rb := &RTCPReportBlock{SSRC: 2}
	packet := BuildGroupSenderReport(1, RTCPSenderStats{}, rb, RTCPGroupReportExtension{}, 0)
	for off := 0; off+1 < len(packet); off += 4 {
		if packet[off+1] == RTCPPayloadTypeSDES {
			t.Fatalf("found an SDES packet (PT %d) at offset %d", RTCPPayloadTypeSDES, off)
		}
	}
}

// Sem bloco de recepcao nao ha o que estender: o pacote e o SR simples.
func TestBuildGroupSenderReportWithoutBlock(t *testing.T) {
	ext := RTCPGroupReportExtension{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	packet := BuildGroupSenderReport(1, RTCPSenderStats{}, nil, ext, 0)
	if len(packet) != 28 {
		t.Fatalf("packet = %d bytes, want the plain 28-byte SR", len(packet))
	}
	if rc := packet[0] & 0x1f; rc != 0 {
		t.Errorf("report count = %d, want 0", rc)
	}
}
