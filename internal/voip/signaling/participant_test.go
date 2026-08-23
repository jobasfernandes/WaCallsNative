package signaling

import (
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

func TestRaiseHandRoundTrip(t *testing.T) {
	to := types.NewJID("5511999990000", types.DefaultUserServer)
	creator := types.NewJID("5511888880000", types.DefaultUserServer)
	for _, raised := range []bool{true, false} {
		node := BuildRaiseHand("CALL1", to, creator, "REQ1", raised)
		children := node.GetChildren()
		if len(children) != 1 || children[0].Tag != "user_action" {
			t.Fatalf("children = %v, want one user_action", children)
		}
		action := children[0]
		if action.Attrs["action"] != "raise_hand" {
			t.Errorf("action = %v, want raise_hand", action.Attrs["action"])
		}
		got, err := ParseRaiseHand(&action)
		if err != nil {
			t.Fatalf("ParseRaiseHand: %v", err)
		}
		if got != raised {
			t.Errorf("raised = %v, want %v", got, raised)
		}
	}
}

func TestParseRaiseHandRejectsInvalid(t *testing.T) {
	creator := types.NewJID("5511888880000", types.DefaultUserServer)
	cases := []struct {
		name string
		node *waBinary.Node
	}{
		{"nil", nil},
		{"wrong tag", &waBinary.Node{Tag: "screen_share"}},
		{"wrong action", &waBinary.Node{
			Tag:   "user_action",
			Attrs: waBinary.Attrs{"action": "other", "call-id": "C", "call-creator": creator},
		}},
		{"missing child", &waBinary.Node{
			Tag:   "user_action",
			Attrs: waBinary.Attrs{"action": "raise_hand", "call-id": "C", "call-creator": creator},
		}},
		{"invalid state", &waBinary.Node{
			Tag:   "user_action",
			Attrs: waBinary.Attrs{"action": "raise_hand", "call-id": "C", "call-creator": creator},
			Content: []waBinary.Node{
				{Tag: "raise_hand", Attrs: waBinary.Attrs{"raise-hand-state": "7"}},
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseRaiseHand(tc.node); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestScreenShareRoundTrip(t *testing.T) {
	to := types.NewJID("5511999990000", types.DefaultUserServer)
	creator := types.NewJID("5511888880000", types.DefaultUserServer)
	id := uint32(4242)
	node := BuildScreenShare("CALL1", to, creator, "REQ1", ScreenShareStarted, &id)
	children := node.GetChildren()
	if len(children) != 1 || children[0].Tag != "screen_share" {
		t.Fatalf("children = %v, want one screen_share", children)
	}
	share := children[0]
	// A versao 2 e o que distingue a forma capturada da anterior.
	if share.Attrs["version"] != "2" {
		t.Errorf("version = %v, want 2", share.Attrs["version"])
	}
	got, err := ParseScreenShare(&share)
	if err != nil {
		t.Fatalf("ParseScreenShare: %v", err)
	}
	if got.State != ScreenShareStarted {
		t.Errorf("state = %v, want started", got.State)
	}
	if !got.HasScreenShareID || got.ScreenShareID != id {
		t.Errorf("screenShareID = %v (has=%v), want %d", got.ScreenShareID, got.HasScreenShareID, id)
	}
}

// Sem screenShareID o atributo nao pode ser emitido, e o parse marca HasScreenShareID=false.
func TestScreenShareWithoutID(t *testing.T) {
	to := types.NewJID("5511999990000", types.DefaultUserServer)
	creator := types.NewJID("5511888880000", types.DefaultUserServer)
	node := BuildScreenShare("CALL1", to, creator, "REQ1", ScreenShareStopped, nil)
	share := node.GetChildren()[0]
	if _, present := share.Attrs["screen_share_id"]; present {
		t.Error("screen_share_id must be absent when not supplied")
	}
	got, err := ParseScreenShare(&share)
	if err != nil {
		t.Fatalf("ParseScreenShare: %v", err)
	}
	if got.HasScreenShareID {
		t.Error("HasScreenShareID must be false")
	}
	if got.State != ScreenShareStopped {
		t.Errorf("state = %v, want stopped", got.State)
	}
}

func TestParseScreenShareRejectsUnsupportedState(t *testing.T) {
	node := &waBinary.Node{
		Tag:   "screen_share",
		Attrs: waBinary.Attrs{"screenshare_state": "9", "version": "2"},
	}
	if _, err := ParseScreenShare(node); err == nil {
		t.Error("expected an error for an unsupported state")
	}
}
