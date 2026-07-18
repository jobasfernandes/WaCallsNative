package signaling

import (
	"testing"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/wanode"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

func videoTestJID(t *testing.T, s string) types.JID {
	t.Helper()
	j, err := types.ParseJID(s)
	if err != nil {
		t.Fatalf("parse jid %s: %v", s, err)
	}
	return j
}

func TestBuildVideoStateStanzaUpgradeRequest(t *testing.T) {
	node := BuildVideoStateStanza(VideoStateParams{
		CallID: "C1", To: videoTestJID(t, "1@lid"), CallCreator: videoTestJID(t, "2@lid"),
		State: core.VideoStateUpgradeRequestV2, Dec: VideoDecRequest,
	})
	if node.Tag != "call" {
		t.Fatalf("wrapper tag = %s", node.Tag)
	}
	kids := wanode.NodeChildren(&node)
	if len(kids) != 1 || kids[0].Tag != "video" {
		t.Fatalf("expected single video child, got %+v", kids)
	}
	attrs := kids[0].Attrs
	if wanode.AttrString(attrs, "state") != "11" {
		t.Errorf("state = %s, want 11", wanode.AttrString(attrs, "state"))
	}
	if wanode.AttrString(attrs, "dec") != "H264" {
		t.Errorf("dec = %s, want H264", wanode.AttrString(attrs, "dec"))
	}
	if wanode.AttrString(attrs, "voip_settings") != "video" {
		t.Errorf("state 11 must carry voip_settings=video")
	}
	if _, ok := attrs["device_orientation"]; ok {
		t.Errorf("orientation must be absent when nil")
	}
	if wanode.AttrString(attrs, "call-id") != "C1" {
		t.Errorf("call-id = %s", wanode.AttrString(attrs, "call-id"))
	}
}

func TestBuildVideoStateStanzaStop(t *testing.T) {
	o := 0
	node := BuildVideoStateStanza(VideoStateParams{
		CallID: "C1", To: videoTestJID(t, "1@lid"), CallCreator: videoTestJID(t, "2@lid"),
		State: core.VideoStateStopped, DeviceOrientation: &o,
	})
	attrs := wanode.NodeChildren(&node)[0].Attrs
	if wanode.AttrString(attrs, "state") != "6" {
		t.Errorf("state = %s, want 6", wanode.AttrString(attrs, "state"))
	}
	if wanode.AttrString(attrs, "device_orientation") != "0" {
		t.Errorf("orientation must be 0")
	}
	if _, ok := attrs["voip_settings"]; ok {
		t.Errorf("voip_settings only on state 11")
	}
	if _, ok := attrs["dec"]; ok {
		t.Errorf("dec must be absent when empty")
	}
}

func TestBuildVideoAckEchoesRouting(t *testing.T) {
	from := videoTestJID(t, "1.1:77@lid")
	participant := videoTestJID(t, "3.3:12@lid")
	recipient := videoTestJID(t, "4@s.whatsapp.net")
	orig := waBinary.Node{Tag: "call", Attrs: waBinary.Attrs{
		"id": "ID9", "from": from, "participant": participant, "recipient": recipient,
	}}
	ack, ok := BuildVideoAck(&orig)
	if !ok {
		t.Fatal("BuildVideoAck not ok")
	}
	if ack.Tag != "ack" {
		t.Fatalf("tag = %s", ack.Tag)
	}
	if wanode.AttrString(ack.Attrs, "class") != "call" || wanode.AttrString(ack.Attrs, "type") != "video" {
		t.Errorf("ack must be class=call type=video, got %+v", ack.Attrs)
	}
	if wanode.AttrString(ack.Attrs, "id") != "ID9" {
		t.Errorf("id not echoed")
	}
	if ack.Attrs["to"] != from {
		t.Errorf("to must be original from")
	}
	if ack.Attrs["participant"] != participant {
		t.Errorf("participant not echoed")
	}
	if ack.Attrs["recipient"] != recipient {
		t.Errorf("recipient not echoed")
	}
}

func TestBuildVideoAckOmitsParticipantEqualFrom(t *testing.T) {
	from := videoTestJID(t, "1.1:77@lid")
	orig := waBinary.Node{Tag: "call", Attrs: waBinary.Attrs{"id": "ID9", "from": from, "participant": from}}
	ack, ok := BuildVideoAck(&orig)
	if !ok {
		t.Fatal("not ok")
	}
	if _, present := ack.Attrs["participant"]; present {
		t.Errorf("participant == from must be omitted")
	}
}

func TestBuildVideoAckRejectsMissingIDOrFrom(t *testing.T) {
	if _, ok := BuildVideoAck(&waBinary.Node{Tag: "call", Attrs: waBinary.Attrs{"id": "X"}}); ok {
		t.Errorf("missing from must not ack")
	}
	if _, ok := BuildVideoAck(&waBinary.Node{Tag: "call", Attrs: waBinary.Attrs{"from": videoTestJID(t, "1@lid")}}); ok {
		t.Errorf("missing id must not ack")
	}
}

func TestOfferHasVideo(t *testing.T) {
	withVideo := waBinary.Node{Tag: "offer", Content: []waBinary.Node{{Tag: "audio"}, {Tag: "video"}}}
	audioOnly := waBinary.Node{Tag: "offer", Content: []waBinary.Node{{Tag: "audio"}}}
	if !OfferHasVideo(&withVideo) {
		t.Error("with video -> false")
	}
	if OfferHasVideo(&audioOnly) {
		t.Error("audio only -> true")
	}
	if OfferHasVideo(nil) {
		t.Error("nil -> true")
	}
}

func TestParseVideoState(t *testing.T) {
	inner := waBinary.Node{Tag: "video", Attrs: waBinary.Attrs{"state": "4", "device_orientation": "2"}}
	state, orientation := ParseVideoState(&inner)
	if state != core.VideoStateUpgradeAccept || orientation != 2 {
		t.Errorf("got state=%d orientation=%d", state, orientation)
	}
	noOrient := waBinary.Node{Tag: "video", Attrs: waBinary.Attrs{"state": "1"}}
	state, orientation = ParseVideoState(&noOrient)
	if state != core.VideoStateEnabled || orientation != 0 {
		t.Errorf("defaults wrong: state=%d orientation=%d", state, orientation)
	}
}

func TestVideoAdvertisementNodes(t *testing.T) {
	offer := videoOfferNode()
	if wanode.AttrString(offer.Attrs, "enc") != "h.264" || wanode.AttrString(offer.Attrs, "dec") != "H264" {
		t.Errorf("offer node spelling wrong: %+v", offer.Attrs)
	}
	if wanode.AttrString(offer.Attrs, "screen_width") != "1920" || wanode.AttrString(offer.Attrs, "screen_height") != "1080" {
		t.Errorf("offer screen dims wrong")
	}
	accept := videoAcceptNode()
	if wanode.AttrString(accept.Attrs, "enc") != "h.264" || wanode.AttrString(accept.Attrs, "dec") != "H264,H265,AV1" {
		t.Errorf("accept node attrs wrong: %+v", accept.Attrs)
	}
	pre := videoPreacceptNode()
	if wanode.AttrString(pre.Attrs, "screen_width") != "0" || wanode.AttrString(pre.Attrs, "screen_height") != "0" {
		t.Errorf("preaccept screen dims must be 0")
	}
}
