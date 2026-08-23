package signaling

import (
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

func TestBuildCallControlAck(t *testing.T) {
	from := types.NewJID("5511999990000", types.DefaultUserServer)
	original := &waBinary.Node{
		Tag:   "call",
		Attrs: waBinary.Attrs{"id": "STANZA1", "from": from},
	}
	ack, ok := BuildCallControlAck(original, "group_update")
	if !ok {
		t.Fatal("a node with id and from must produce an ack")
	}
	if ack.Tag != "ack" {
		t.Errorf("tag = %q, want ack", ack.Tag)
	}
	// O type e o que torna o ack aceitavel: sem ele o peer reverte o estado.
	for k, want := range map[string]any{
		"class": "call", "id": "STANZA1", "to": from, "type": "group_update",
	} {
		if got := ack.Attrs[k]; got != want {
			t.Errorf("attr %q = %v, want %v", k, got, want)
		}
	}
	if _, present := ack.Attrs["participant"]; present {
		t.Error("participant must be absent when the original has none")
	}
}

// participant e recipient tem de ser propagados, senao o servidor entrega o ack
// ao device errado numa chamada multi-device.
func TestBuildCallControlAckPropagatesRouting(t *testing.T) {
	from := types.NewJID("5511999990000", types.DefaultUserServer)
	participant := types.NewJID("5511888880000", types.DefaultUserServer)
	recipient := types.NewJID("5511777770000", types.DefaultUserServer)
	original := &waBinary.Node{
		Tag: "call",
		Attrs: waBinary.Attrs{
			"id": "STANZA2", "from": from,
			"participant": participant, "recipient": recipient,
		},
	}
	ack, ok := BuildCallControlAck(original, "enc_rekey")
	if !ok {
		t.Fatal("expected an ack")
	}
	if ack.Attrs["participant"] != participant {
		t.Errorf("participant = %v, want %v", ack.Attrs["participant"], participant)
	}
	if ack.Attrs["recipient"] != recipient {
		t.Errorf("recipient = %v, want %v", ack.Attrs["recipient"], recipient)
	}
}

// Quando participant == from ele e redundante e nao deve ser emitido.
func TestBuildCallControlAckOmitsRedundantParticipant(t *testing.T) {
	from := types.NewJID("5511999990000", types.DefaultUserServer)
	original := &waBinary.Node{
		Tag:   "call",
		Attrs: waBinary.Attrs{"id": "STANZA3", "from": from, "participant": from},
	}
	ack, ok := BuildCallControlAck(original, "user_action")
	if !ok {
		t.Fatal("expected an ack")
	}
	if _, present := ack.Attrs["participant"]; present {
		t.Error("participant equal to from must be omitted")
	}
}

func TestBuildCallControlAckRejectsIncomplete(t *testing.T) {
	from := types.NewJID("5511999990000", types.DefaultUserServer)
	cases := []struct {
		name     string
		node     *waBinary.Node
		childTag string
	}{
		{"nil node", nil, "group_update"},
		{"empty child tag", &waBinary.Node{Attrs: waBinary.Attrs{"id": "X", "from": from}}, ""},
		{"missing id", &waBinary.Node{Attrs: waBinary.Attrs{"from": from}}, "group_update"},
		{"missing from", &waBinary.Node{Attrs: waBinary.Attrs{"id": "X"}}, "group_update"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := BuildCallControlAck(tc.node, tc.childTag); ok {
				t.Error("expected no ack")
			}
		})
	}
}

func TestCallWrapWithIDUsesTheGivenID(t *testing.T) {
	to := types.NewJID("5511999990000", types.DefaultUserServer)
	node := callWrapWithID(to, "FIXEDID", waBinary.Node{Tag: "user_action"})
	if node.Tag != "call" {
		t.Errorf("tag = %q, want call", node.Tag)
	}
	if node.Attrs["id"] != "FIXEDID" {
		t.Errorf("id = %v, want FIXEDID; request/response correlation depends on it", node.Attrs["id"])
	}
	if node.Attrs["to"] != to {
		t.Errorf("to = %v, want %v", node.Attrs["to"], to)
	}
	children := node.GetChildren()
	if len(children) != 1 || children[0].Tag != "user_action" {
		t.Fatalf("children = %v, want one user_action", children)
	}
}
