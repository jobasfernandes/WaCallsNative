package transport

import (
	"bytes"
	"testing"
)

func TestBuildSenderSubscriptionsAudioOnlyByteIdentical(t *testing.T) {
	// Audio-only single entry must equal the pre-F2 wire bytes (layer 0, pt 0).
	got := BuildSenderSubscriptions(SubEntry{SSRC: 0x12345678, StreamLayer: 0, PayloadType: 0})
	want := encodeProtobufLengthDelimited(1, concat(
		encodeProtobufVarintField(3, 0x12345678),
		encodeProtobufVarintField(5, 0),
		encodeProtobufVarintField(6, 0),
	))
	if !bytes.Equal(got, want) {
		t.Fatalf("audio subscription bytes changed:\n got %x\nwant %x", got, want)
	}
}

func TestBuildSenderSubscriptionsVideoLayer1(t *testing.T) {
	got := BuildSenderSubscriptions(
		SubEntry{SSRC: 0xAAAA, StreamLayer: 0, PayloadType: 0},
		SubEntry{SSRC: 0xBBBB, StreamLayer: 1, PayloadType: 0},
	)
	audio := encodeProtobufLengthDelimited(1, concat(
		encodeProtobufVarintField(3, 0xAAAA), encodeProtobufVarintField(5, 0), encodeProtobufVarintField(6, 0)))
	video := encodeProtobufLengthDelimited(1, concat(
		encodeProtobufVarintField(3, 0xBBBB), encodeProtobufVarintField(5, 1), encodeProtobufVarintField(6, 0)))
	want := concat(audio, video)
	if !bytes.Equal(got, want) {
		t.Fatalf("multi-entry subscription bytes wrong:\n got %x\nwant %x", got, want)
	}
}
