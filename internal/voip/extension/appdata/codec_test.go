package appdata

import (
	"bytes"
	"testing"
)

// Vetores byte-exatos de trafego capturado. A estrutura e:
//
//	Payloads { repeated Message payloads = 1 }
//	Message  { Reaction reaction = 1 }
//	Reaction { uint64 transaction_id = 1; string emoji = 2 }
func TestEncodeReactionMatchesCapturedBytes(t *testing.T) {
	cases := []struct {
		name  string
		txID  uint64
		emoji string
		want  []byte
	}{
		{
			name:  "thumbs up",
			txID:  1,
			emoji: "\U0001F44D",
			want:  []byte{0x0a, 0x0a, 0x0a, 0x08, 0x08, 0x01, 0x12, 0x04, 0xf0, 0x9f, 0x91, 0x8d},
		},
		{
			name:  "cleared reaction",
			txID:  2,
			emoji: "",
			want:  []byte{0x0a, 0x06, 0x0a, 0x04, 0x08, 0x02, 0x12, 0x00},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeReaction(tc.txID, tc.emoji)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("encodeReaction(%d, %q) = % x, want % x", tc.txID, tc.emoji, got, tc.want)
			}
		})
	}
}

func TestDecodeReactionsRoundTrip(t *testing.T) {
	got, err := decodeReactions(encodeReaction(7, "❤️"))
	if err != nil {
		t.Fatalf("decodeReactions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d reactions, want 1", len(got))
	}
	if got[0].transactionID != 7 {
		t.Errorf("transactionID = %d, want 7", got[0].transactionID)
	}
	if got[0].emoji != "❤️" {
		t.Errorf("emoji = %q, want heart", got[0].emoji)
	}
}

// Um campo desconhecido no nivel Reaction nao pode derrubar o decode: o protocolo
// e de terceiro e pode ganhar campos sem aviso.
func TestDecodeReactionsSkipsUnknownFields(t *testing.T) {
	payload := []byte{
		0x0a, 0x0c, // Payloads.payloads, len 12
		0x0a, 0x0a, // Message.reaction, len 10
		0x08, 0x03, // transaction_id = 3
		0x18, 0x2a, // field 3 varint 42, desconhecido
		0x12, 0x04, 0xf0, 0x9f, 0x91, 0x8d, // emoji
	}
	got, err := decodeReactions(payload)
	if err != nil {
		t.Fatalf("decodeReactions: %v", err)
	}
	if len(got) != 1 || got[0].transactionID != 3 {
		t.Fatalf("got %+v, want one reaction with transactionID 3", got)
	}
	if got[0].emoji != "\U0001F44D" {
		t.Errorf("emoji = %q, want thumbs up", got[0].emoji)
	}
}

func TestDecodeReactionsRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"truncated", []byte{0x0a, 0x0a, 0x0a}},
		{"empty", []byte{}},
		// transaction_id 0 nunca e emitido: e a sentinela de campo ausente.
		{"transaction id zero", []byte{0x0a, 0x06, 0x0a, 0x04, 0x08, 0x00, 0x12, 0x00}},
		// emoji com um byte 0xff solto nao e UTF-8 valido.
		{"invalid utf8 emoji", []byte{0x0a, 0x07, 0x0a, 0x05, 0x08, 0x01, 0x12, 0x01, 0xff}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeReactions(tc.payload); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}
