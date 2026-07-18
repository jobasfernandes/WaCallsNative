package signaling

import (
	"bytes"
	"context"
	"testing"

	"wacalls/internal/voip/wanode"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

type buildTestSock struct{}

var _ Socket = buildTestSock{}

func (buildTestSock) OwnPN() types.JID  { return types.NewJID("111", types.DefaultUserServer) }
func (buildTestSock) OwnLID() types.JID { return types.NewJID("111", types.HiddenUserServer) }
func (buildTestSock) AccountDeviceIdentityNode() (waBinary.Node, bool) {
	return waBinary.Node{}, false
}
func (buildTestSock) SendNode(ctx context.Context, node waBinary.Node) error { return nil }
func (buildTestSock) Query(ctx context.Context, node waBinary.Node) (*waBinary.Node, error) {
	return nil, nil
}
func (buildTestSock) GetUSyncDevices(ctx context.Context, jids []types.JID) ([]types.JID, error) {
	return jids, nil
}
func (buildTestSock) AssertSessions(ctx context.Context, jids []types.JID, force bool) error {
	return nil
}
func (buildTestSock) CreateParticipantNodes(ctx context.Context, devices []types.JID, callKey []byte, encAttrs waBinary.Attrs) ([]waBinary.Node, bool, error) {
	return []waBinary.Node{{Tag: "enc", Attrs: waBinary.Attrs{"v": "2", "type": "msg"}}}, false, nil
}
func (buildTestSock) DecryptCallKey(ctx context.Context, from types.JID, encChild *waBinary.Node) ([]byte, error) {
	return nil, nil
}
func (buildTestSock) GetTCToken(ctx context.Context, jid types.JID) ([]byte, error) {
	return nil, nil
}
func (buildTestSock) ResolveLIDForPN(ctx context.Context, pn types.JID) types.JID { return pn }

func offerChildren(t *testing.T, node waBinary.Node) []waBinary.Node {
	t.Helper()
	kids := wanode.NodeChildren(&node)
	if len(kids) != 1 {
		t.Fatalf("wrapper children = %d, want 1", len(kids))
	}
	return wanode.NodeChildren(&kids[0])
}

func TestBuildOfferStanzaVideo(t *testing.T) {
	peer := types.NewJID("62440234549366", types.HiddenUserServer)
	node, _, err := BuildOfferStanza(context.Background(), buildTestSock{}, "C1", []byte("k"), peer, true)
	if err != nil {
		t.Fatal(err)
	}
	tags := []string{}
	var capContent []byte
	var video *waBinary.Node
	for _, c := range offerChildren(t, node) {
		tags = append(tags, c.Tag)
		if c.Tag == "capability" {
			capContent = c.Content.([]byte)
		}
		if c.Tag == "video" {
			cc := c
			video = &cc
		}
	}
	if video == nil {
		t.Fatalf("no video child in video offer: %v", tags)
	}
	if wanode.AttrString(video.Attrs, "enc") != "h.264" || wanode.AttrString(video.Attrs, "dec") != "H264" {
		t.Errorf("video child spelling wrong: %+v", video.Attrs)
	}
	want := []byte{0x01, 0x05, 0xf7, 0x09, 0xe4, 0xfa, 0x13}
	if !bytes.Equal(capContent, want) {
		t.Errorf("video capability = %x, want %x", capContent, want)
	}
	for i, tag := range tags {
		if tag == "video" {
			if tags[i-1] != "audio" || tags[i+1] != "net" {
				t.Errorf("video misplaced between %q and %q: %v", tags[i-1], tags[i+1], tags)
			}
		}
	}
}

func TestBuildOfferStanzaAudioUnchanged(t *testing.T) {
	peer := types.NewJID("62440234549366", types.HiddenUserServer)
	node, _, err := BuildOfferStanza(context.Background(), buildTestSock{}, "C1", []byte("k"), peer, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range offerChildren(t, node) {
		if c.Tag == "video" {
			t.Fatal("audio offer must not carry video child")
		}
		if c.Tag == "capability" {
			want := []byte{0x01, 0x05, 0xf7, 0x09, 0xe4, 0xbb, 0x13}
			if !bytes.Equal(c.Content.([]byte), want) {
				t.Errorf("audio capability changed: %x", c.Content.([]byte))
			}
		}
	}
}

func TestBuildAcceptStanzaVideo(t *testing.T) {
	peer := types.NewJID("62440234549366", types.HiddenUserServer)
	creator := types.NewJID("111", types.HiddenUserServer)
	node, err := BuildAcceptStanza(context.Background(), buildTestSock{}, "C1", []byte("k"), peer, creator, true)
	if err != nil {
		t.Fatal(err)
	}
	kids := offerChildren(t, node)
	tags := []string{}
	for _, c := range kids {
		tags = append(tags, c.Tag)
	}
	// The video advertisement sits at the END of the accept content (after encopt), matching
	// the official client; audio stays first.
	if len(tags) < 2 || tags[0] != "audio" || tags[len(tags)-1] != "video" {
		t.Errorf("video must be the last accept child, after audio: %v", tags)
	}
	last := kids[len(kids)-1]
	if wanode.AttrString(last.Attrs, "enc") != "h.264" || wanode.AttrString(last.Attrs, "dec") != "H264,H265,AV1" {
		t.Errorf("accept video node attrs wrong: %+v", last.Attrs)
	}
}

func TestBuildAcceptStanzaAudioUnchanged(t *testing.T) {
	peer := types.NewJID("62440234549366", types.HiddenUserServer)
	creator := types.NewJID("111", types.HiddenUserServer)
	node, err := BuildAcceptStanza(context.Background(), buildTestSock{}, "C1", []byte("k"), peer, creator, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range offerChildren(t, node) {
		if c.Tag == "video" {
			t.Fatal("audio accept must not carry video child")
		}
	}
}

func TestBuildPreacceptStanzaVideo(t *testing.T) {
	peer := types.NewJID("62440234549366", types.HiddenUserServer)
	creator := types.NewJID("111", types.HiddenUserServer)
	node := BuildPreacceptStanza(peer, "C1", creator, true)
	for _, c := range offerChildren(t, node) {
		if c.Tag == "video" {
			// The video preaccept advertises capability only (no <video> child), matching the
			// official client.
			t.Error("video preaccept must not carry a <video> child")
		}
		if c.Tag == "capability" {
			want := []byte{0x01, 0x05, 0xff, 0x09, 0xe0, 0xfa, 0x13}
			if !bytes.Equal(c.Content.([]byte), want) {
				t.Errorf("video preaccept capability = %x, want %x", c.Content.([]byte), want)
			}
		}
	}
}

func TestBuildPreacceptStanzaAudioUnchanged(t *testing.T) {
	peer := types.NewJID("62440234549366", types.HiddenUserServer)
	creator := types.NewJID("111", types.HiddenUserServer)
	node := BuildPreacceptStanza(peer, "C1", creator, false)
	for _, c := range offerChildren(t, node) {
		if c.Tag == "video" {
			t.Fatal("audio preaccept must not carry video")
		}
		if c.Tag == "capability" {
			want := []byte{0x01, 0x05, 0xf7, 0x09, 0xe4, 0xbb, 0x07}
			if !bytes.Equal(c.Content.([]byte), want) {
				t.Errorf("audio preaccept blob changed: %x", c.Content.([]byte))
			}
		}
	}
}

func TestBuildTerminateElsewhereStanza(t *testing.T) {
	peer := types.NewJID("62440234549366", types.HiddenUserServer)
	creator := types.NewJID("111", types.HiddenUserServer)
	dev0 := types.NewJID("62440234549366", types.HiddenUserServer)
	dev0.Device = 0
	dev5 := types.NewJID("62440234549366", types.HiddenUserServer)
	dev5.Device = 5

	node := BuildTerminateElsewhereStanza(peer, "CID", creator, []types.JID{dev0, dev5})

	if node.Tag != "call" {
		t.Fatalf("wrapper tag = %q", node.Tag)
	}
	children := wanode.NodeChildren(&node)
	if len(children) != 1 || children[0].Tag != "terminate" {
		t.Fatalf("want single terminate child, got %+v", children)
	}
	term := children[0]
	if r := wanode.AttrString(term.Attrs, "reason"); r != "accepted_elsewhere" {
		t.Fatalf("reason = %q, want accepted_elsewhere", r)
	}
	if id := wanode.AttrString(term.Attrs, "call-id"); id != "CID" {
		t.Fatalf("call-id = %q", id)
	}
	dst := wanode.FindChildByTag(&term, "destination")
	if dst == nil {
		t.Fatal("destination missing")
	}
	tos := wanode.NodeChildren(dst)
	if len(tos) != 2 {
		t.Fatalf("want 2 destination devices, got %d", len(tos))
	}
	if j := wanode.AttrString(tos[1].Attrs, "jid"); j != dev5.String() {
		t.Fatalf("second destination = %q, want %q", j, dev5)
	}
}
