package call

import (
	"context"
	"log/slog"
	"testing"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/engine"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

func videoOfferNode(callID string, from types.JID) *waBinary.Node {
	return &waBinary.Node{
		Tag:   "call",
		Attrs: waBinary.Attrs{"from": from},
		Content: []waBinary.Node{{
			Tag:   "offer",
			Attrs: waBinary.Attrs{"call-id": callID, "call-creator": from.String()},
			Content: []waBinary.Node{
				{Tag: "audio", Attrs: waBinary.Attrs{"enc": "opus", "rate": "16000"}},
				{Tag: "video", Attrs: waBinary.Attrs{"enc": "h.264", "dec": "H264"}},
			},
		}},
	}
}

func TestStartCallVideoSetsMediaTypeAndOffer(t *testing.T) {
	sock := &recVideoSock{}
	cm := NewCallManager(sock, slog.Default())
	peer := types.NewJID("5511999990000", types.DefaultUserServer)

	if err := cm.StartCall(context.Background(), "CALL1", peer, true); err != nil {
		t.Fatalf("start: %v", err)
	}
	if cm.CurrentCall().MediaType != core.CallMediaTypeVideo {
		t.Errorf("MediaType = %s, want video", cm.CurrentCall().MediaType)
	}
	if !cm.localVideo || !cm.remoteVideo || cm.videoGate {
		t.Errorf("video-from-offer flags wrong: local=%v remote=%v gate=%v", cm.localVideo, cm.remoteVideo, cm.videoGate)
	}
	// StartCall sends the offer via Query, not SendNode; recVideoSock records SendNode,
	// so assert on the offer node the manager built by inspecting sent Query is not
	// applicable here. Instead verify the media type + flags (wire shape covered in
	// signaling_build_test.go TestBuildOfferStanzaVideo).
}

func TestStartCallAudioDefaults(t *testing.T) {
	sock := &recVideoSock{}
	cm := NewCallManager(sock, slog.Default())
	peer := types.NewJID("5511999990000", types.DefaultUserServer)

	if err := cm.StartCall(context.Background(), "CALL1", peer, false); err != nil {
		t.Fatalf("start: %v", err)
	}
	if cm.CurrentCall().MediaType != core.CallMediaTypeAudio {
		t.Errorf("MediaType = %s, want audio", cm.CurrentCall().MediaType)
	}
	if cm.localVideo || cm.remoteVideo || cm.videoGate {
		t.Error("audio call must have all video flags false")
	}
}

func TestHandleCallOfferVideoDetected(t *testing.T) {
	sock := &recVideoSock{}
	c := NewClient(sock, slog.Default(), func() []engine.Extension { return nil }, 0, func(string, *CallManager) {}, nil)
	peer := types.NewJID("5511999990000", types.DefaultUserServer)
	c.HandleOffer(context.Background(), videoOfferNode("CALL1", peer), peer)

	cm, ok := c.Get("CALL1")
	if !ok {
		t.Fatal("offer must register a call manager")
	}
	if cm.CurrentCall().MediaType != core.CallMediaTypeVideo {
		t.Errorf("inbound video offer must set MediaType video, got %s", cm.CurrentCall().MediaType)
	}
	if !cm.localVideo || !cm.remoteVideo {
		t.Errorf("inbound video offer must set local+remote video: local=%v remote=%v", cm.localVideo, cm.remoteVideo)
	}
}

func TestHandleCallOfferAudioUnchanged(t *testing.T) {
	sock := &recVideoSock{}
	c := NewClient(sock, slog.Default(), func() []engine.Extension { return nil }, 0, func(string, *CallManager) {}, nil)
	peer := types.NewJID("5511999990000", types.DefaultUserServer)
	c.HandleOffer(context.Background(), offerNode("CALL1", peer), peer)

	cm, ok := c.Get("CALL1")
	if !ok {
		t.Fatal("offer must register a call manager")
	}
	if cm.CurrentCall().MediaType != core.CallMediaTypeAudio {
		t.Errorf("audio offer must set MediaType audio, got %s", cm.CurrentCall().MediaType)
	}
	if cm.localVideo || cm.remoteVideo {
		t.Error("audio offer must leave video flags false")
	}
}
