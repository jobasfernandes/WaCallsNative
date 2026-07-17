package call

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/wanode"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

type sentVideoStanza struct {
	tag        string
	to         string
	childAttrs waBinary.Attrs
}

// recVideoSock records outbound SendNode stanzas; video transitions send synchronously
// (not via the async Query path mute uses), so the state machine can roll back on error.
type recVideoSock struct {
	fakeSock
	mu     sync.Mutex
	sent   []sentVideoStanza
	failOn int // 1-based index of the SendNode call that fails; 0 = never
}

func (s *recVideoSock) SendNode(ctx context.Context, node waBinary.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rec sentVideoStanza
	if kids := wanode.NodeChildren(&node); len(kids) > 0 {
		rec.tag = kids[0].Tag
		rec.childAttrs = kids[0].Attrs
	}
	if j, ok := node.Attrs["to"].(types.JID); ok {
		rec.to = j.String()
	}
	s.sent = append(s.sent, rec)
	if s.failOn != 0 && len(s.sent) == s.failOn {
		return errors.New("send failed")
	}
	return nil
}

func (s *recVideoSock) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = nil
}

func (s *recVideoSock) videoStates() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, st := range s.sent {
		if st.tag == "video" {
			out = append(out, wanode.AttrString(st.childAttrs, "state"))
		}
	}
	return out
}

func (s *recVideoSock) lastVideo() (sentVideoStanza, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range slices.Backward(s.sent) {
		if st.tag == "video" {
			return st, true
		}
	}
	return sentVideoStanza{}, false
}

func videoCallNode(callID string, state int, from types.JID) *waBinary.Node {
	return &waBinary.Node{
		Tag:   "call",
		Attrs: waBinary.Attrs{"from": from},
		Content: []waBinary.Node{{
			Tag: "video",
			Attrs: waBinary.Attrs{
				"call-id": callID, "call-creator": from.String(),
				"state": strconv.Itoa(state),
			},
		}},
	}
}

func lastVideoSnapshot(snaps []core.VideoSnapshot) core.VideoSnapshot {
	if len(snaps) == 0 {
		return core.VideoSnapshot{}
	}
	return snaps[len(snaps)-1]
}

func TestRequestVideoUpgradeSendsState11(t *testing.T) {
	sock := &recVideoSock{}
	cm := activeManager(t, sock)
	sock.reset()

	var snaps []core.VideoSnapshot
	cm.OnVideoState = func(_ string, s core.VideoSnapshot) { snaps = append(snaps, s) }

	if err := cm.RequestVideoUpgrade(context.Background()); err != nil {
		t.Fatalf("request: %v", err)
	}
	got, ok := sock.lastVideo()
	if !ok || wanode.AttrString(got.childAttrs, "state") != "11" {
		t.Fatalf("want a state=11 video stanza, got %+v", sock.videoStates())
	}
	if wanode.AttrString(got.childAttrs, "dec") != "H264" {
		t.Errorf("dec = %s, want H264", wanode.AttrString(got.childAttrs, "dec"))
	}
	if !cm.localVideo || !cm.videoGate {
		t.Errorf("localVideo/videoGate must be true after request")
	}
	if s := lastVideoSnapshot(snaps); s.Pending != "out" {
		t.Errorf("snapshot Pending = %q, want out", s.Pending)
	}
}

func TestRequestVideoUpgradeRollbackOnSendFailure(t *testing.T) {
	sock := &recVideoSock{}
	cm := activeManager(t, sock)
	sock.reset()
	sock.failOn = 1

	if err := cm.RequestVideoUpgrade(context.Background()); err == nil {
		t.Fatal("expected send failure")
	}
	if cm.localVideo || cm.videoGate {
		t.Errorf("state must roll back on send failure: local=%v gate=%v", cm.localVideo, cm.videoGate)
	}
}

func TestRequestVideoUpgradeRequiresConnected(t *testing.T) {
	sock := &recVideoSock{}
	cm := ringingManager(t, sock)
	sock.reset()

	err := cm.RequestVideoUpgrade(context.Background())
	var callErr *CallError
	if !errors.As(err, &callErr) {
		t.Fatalf("ringing call must reject upgrade, got %v", err)
	}
	if len(sock.videoStates()) != 0 {
		t.Error("no video stanza must reach the wire")
	}
}

func TestAcceptVideoUpgradeSends4Then1(t *testing.T) {
	sock := &recVideoSock{}
	cm := activeManager(t, sock)
	peer := types.NewJID("5511999990000", types.DefaultUserServer)
	cm.HandleVideoStanza(videoCallNode("CALL1", core.VideoStateUpgradeRequestV2, peer))
	sock.reset()

	if err := cm.AcceptVideoUpgrade(context.Background()); err != nil {
		t.Fatalf("accept: %v", err)
	}
	states := sock.videoStates()
	if len(states) != 2 || states[0] != "4" || states[1] != "1" {
		t.Fatalf("want state 4 then 1, got %v", states)
	}
	if cm.videoPendingIn {
		t.Error("videoPendingIn must clear after accept")
	}
	if !cm.localVideo {
		t.Error("localVideo must be true after accept")
	}
}

func TestInboundEnabledUngates(t *testing.T) {
	sock := &recVideoSock{}
	cm := activeManager(t, sock)
	sock.reset()
	if err := cm.RequestVideoUpgrade(context.Background()); err != nil {
		t.Fatalf("request: %v", err)
	}

	var snaps []core.VideoSnapshot
	cm.OnVideoState = func(_ string, s core.VideoSnapshot) { snaps = append(snaps, s) }
	peer := types.NewJID("5511999990000", types.DefaultUserServer)
	cm.HandleVideoStanza(videoCallNode("CALL1", core.VideoStateEnabled, peer))

	if cm.videoGate {
		t.Error("gate must clear when peer enables")
	}
	if !cm.remoteVideo {
		t.Error("remoteVideo must be true")
	}
	if s := lastVideoSnapshot(snaps); s.Pending != "" {
		t.Errorf("snapshot Pending = %q, want empty", s.Pending)
	}
}

func TestInboundUpgradeAcceptAnnouncesEnabled(t *testing.T) {
	sock := &recVideoSock{}
	cm := activeManager(t, sock)
	sock.reset()

	peer := types.NewJID("5511999990000", types.DefaultUserServer)
	cm.HandleVideoStanza(videoCallNode("CALL1", core.VideoStateUpgradeAccept, peer))

	states := sock.videoStates()
	if len(states) != 1 || states[0] != "1" {
		t.Fatalf("upgrade accept must announce state=1, got %v", states)
	}
	if !cm.localVideo {
		t.Error("localVideo must be true after upgrade accept")
	}
}

func TestInboundRejectClearsLocal(t *testing.T) {
	sock := &recVideoSock{}
	cm := activeManager(t, sock)
	sock.reset()
	if err := cm.RequestVideoUpgrade(context.Background()); err != nil {
		t.Fatalf("request: %v", err)
	}

	peer := types.NewJID("5511999990000", types.DefaultUserServer)
	cm.HandleVideoStanza(videoCallNode("CALL1", core.VideoStateUpgradeReject, peer))

	if cm.localVideo || cm.videoGate {
		t.Errorf("reject must clear local/gate: local=%v gate=%v", cm.localVideo, cm.videoGate)
	}
}

func TestInboundRequestFiresCallback(t *testing.T) {
	sock := &recVideoSock{}
	cm := activeManager(t, sock)
	sock.reset()

	fired := ""
	cm.OnVideoUpgradeRequest = func(callID string) { fired = callID }
	peer := types.NewJID("5511999990000", types.DefaultUserServer)
	cm.HandleVideoStanza(videoCallNode("CALL1", core.VideoStateUpgradeRequestV2, peer))

	if fired != "CALL1" {
		t.Errorf("OnVideoUpgradeRequest fired = %q, want CALL1", fired)
	}
	if !cm.videoPendingIn {
		t.Error("videoPendingIn must be true after inbound request")
	}
	if len(sock.videoStates()) != 0 {
		t.Error("inbound request must not auto-send anything from the call layer")
	}
}

func TestGlareMutualAccept(t *testing.T) {
	sock := &recVideoSock{}
	cm := activeManager(t, sock)
	sock.reset()
	if err := cm.RequestVideoUpgrade(context.Background()); err != nil {
		t.Fatalf("request: %v", err)
	}
	sock.reset()

	fired := false
	cm.OnVideoUpgradeRequest = func(string) { fired = true }
	peer := types.NewJID("5511999990000", types.DefaultUserServer)
	cm.HandleVideoStanza(videoCallNode("CALL1", core.VideoStateUpgradeRequestV2, peer))

	if fired {
		t.Error("glare must not fire OnVideoUpgradeRequest")
	}
	if cm.videoGate || !cm.remoteVideo {
		t.Errorf("glare must ungate + set remoteVideo: gate=%v remote=%v", cm.videoGate, cm.remoteVideo)
	}
	states := sock.videoStates()
	if len(states) != 1 || states[0] != "1" {
		t.Fatalf("glare must announce state=1, got %v", states)
	}
}

func TestVideoStanzaUnknownOrEndedCallIgnored(t *testing.T) {
	sock := &recVideoSock{}
	cm := activeManager(t, sock)
	sock.reset()

	fired := false
	cm.OnVideoUpgradeRequest = func(string) { fired = true }
	peer := types.NewJID("5511999990000", types.DefaultUserServer)

	cm.HandleVideoStanza(videoCallNode("OTHER", core.VideoStateUpgradeRequestV2, peer))
	if fired || len(sock.videoStates()) != 0 {
		t.Fatal("stanza for a different call must be ignored")
	}

	if err := cm.EndCall(context.Background(), core.EndCallReasonUserEnded); err != nil {
		t.Fatalf("end: %v", err)
	}
	sock.reset()
	cm.HandleVideoStanza(videoCallNode("CALL1", core.VideoStateUpgradeRequestV2, peer))
	if fired || len(sock.videoStates()) != 0 {
		t.Fatal("stanza on an ended call must be ignored")
	}
}

func TestVideoStanzaFromNonAnsweringDeviceIgnored(t *testing.T) {
	sock := &recVideoSock{}
	cm := activeManager(t, sock)
	cm.mu.Lock()
	cm.acceptedByJid = "62440234549366:33@lid"
	cm.mu.Unlock()
	sock.reset()

	fired := false
	cm.OnVideoUpgradeRequest = func(string) { fired = true }
	other, _ := types.ParseJID("62440234549366:9@lid")
	cm.HandleVideoStanza(videoCallNode("CALL1", core.VideoStateUpgradeRequestV2, other))

	if fired {
		t.Error("video stanza from a non-answering device must be ignored")
	}
}

func TestStopVideoSends6(t *testing.T) {
	sock := &recVideoSock{}
	cm := activeManager(t, sock)
	sock.reset()
	if err := cm.RequestVideoUpgrade(context.Background()); err != nil {
		t.Fatalf("request: %v", err)
	}
	sock.reset()

	if err := cm.StopVideo(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	got, ok := sock.lastVideo()
	if !ok || wanode.AttrString(got.childAttrs, "state") != "6" {
		t.Fatalf("want state=6, got %v", sock.videoStates())
	}
	if wanode.AttrString(got.childAttrs, "device_orientation") != "0" {
		t.Error("stop must carry device_orientation=0")
	}
	if cm.localVideo {
		t.Error("localVideo must be false after stop")
	}
}

func TestSetVideoOrientationRequiresLocalVideo(t *testing.T) {
	sock := &recVideoSock{}
	cm := activeManager(t, sock)
	sock.reset()

	err := cm.SetVideoOrientation(context.Background(), 1)
	var callErr *CallError
	if !errors.As(err, &callErr) {
		t.Fatalf("orientation without local video must error, got %v", err)
	}

	if err := cm.RequestVideoUpgrade(context.Background()); err != nil {
		t.Fatalf("request: %v", err)
	}
	sock.reset()
	if err := cm.SetVideoOrientation(context.Background(), 2); err != nil {
		t.Fatalf("orientation: %v", err)
	}
	got, ok := sock.lastVideo()
	if !ok || wanode.AttrString(got.childAttrs, "device_orientation") != "2" {
		t.Fatalf("want device_orientation=2, got %+v", got.childAttrs)
	}
}

func TestStopVideoWhileGatedCancels(t *testing.T) {
	sock := &recVideoSock{}
	cm := activeManager(t, sock)
	sock.reset()
	if err := cm.RequestVideoUpgrade(context.Background()); err != nil {
		t.Fatalf("request: %v", err)
	}
	if !cm.videoGate {
		t.Fatal("precondition: gate must be set after request")
	}
	sock.reset()

	if err := cm.StopVideo(context.Background()); err != nil {
		t.Fatalf("stop while gated: %v", err)
	}
	if cm.localVideo || cm.videoGate {
		t.Errorf("stop while gated must clear both: local=%v gate=%v", cm.localVideo, cm.videoGate)
	}
	states := sock.videoStates()
	if len(states) != 1 || states[0] != "6" {
		t.Fatalf("want a single state=6 stanza, got %v", states)
	}
}
