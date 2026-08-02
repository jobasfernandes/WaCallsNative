package call

import (
	"context"
	"log/slog"
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"

	"wacalls/internal/voip/core"
)

// stubSocket is a no-op core.VoipSocket. Only the identity accessors matter
// here: HandleCallAccept reaches the socket through ownCredJid() when it
// derives the SRTP keys, and sends a transport stanza we do not assert on.
type stubSocket struct{ lid types.JID }

func (s *stubSocket) OwnPN() types.JID  { return types.JID{} }
func (s *stubSocket) OwnLID() types.JID { return s.lid }
func (s *stubSocket) AccountDeviceIdentityNode() (waBinary.Node, bool) {
	return waBinary.Node{}, false
}
func (s *stubSocket) SendNode(context.Context, waBinary.Node) error { return nil }
func (s *stubSocket) Query(context.Context, waBinary.Node) (*waBinary.Node, error) {
	return nil, nil
}
func (s *stubSocket) GetUSyncDevices(context.Context, []types.JID) ([]types.JID, error) {
	return nil, nil
}
func (s *stubSocket) AssertSessions(context.Context, []types.JID, bool) error { return nil }
func (s *stubSocket) CreateParticipantNodes(context.Context, []types.JID, []byte, waBinary.Attrs) ([]waBinary.Node, bool, error) {
	return nil, false, nil
}
func (s *stubSocket) DecryptCallKey(context.Context, types.JID, *waBinary.Node) ([]byte, error) {
	return nil, nil
}
func (s *stubSocket) GetTCToken(context.Context, types.JID) ([]byte, error) { return nil, nil }
func (s *stubSocket) ResolveLIDForPN(context.Context, types.JID) types.JID  { return types.JID{} }

var _ core.VoipSocket = (*stubSocket)(nil)

// acceptNode builds the minimal <call><accept/></call> stanza HandleCallAccept
// parses. The stub socket's DecryptCallKey returns nil, so the call-key
// decryption branch is skipped and the test stays focused on the anchoring.
func acceptNode(callID string) *waBinary.Node {
	return &waBinary.Node{
		Tag: "call",
		Content: []waBinary.Node{{
			Tag:   "accept",
			Attrs: waBinary.Attrs{"call-id": callID},
		}},
	}
}

// A second companion device accepting the same offer must not move the peer
// the SRTP keys are derived from. initSrtpKeysLocked reads acceptedByJid, so
// overwriting it rekeys a session whose audio is already flowing: every packet
// from the device that actually answered then fails authentication.
func TestSrtpKeysStayAnchoredToTheFirstAcceptingDevice(t *testing.T) {
	const callID = "CID"
	m := NewCallManager(&stubSocket{lid: types.JID{User: "me", Server: types.HiddenUserServer}}, slog.Default())
	call := NewOutgoingCall(callID, "peer@lid", "me@lid", core.CallMediaTypeAudio)
	call.EncryptionKey = make([]byte, 32)
	if err := call.ApplyTransition(Transition{Type: TransitionOfferSent}); err != nil {
		t.Fatalf("offer_sent: %v", err)
	}
	m.currentCall = call

	first := types.JID{User: "peer", Device: 1, Server: types.HiddenUserServer}
	m.HandleCallAccept(context.Background(), acceptNode(callID), first)

	anchored := m.acceptedByJid
	if anchored != first.String() {
		t.Fatalf("first accept must anchor the peer, got %q want %q", anchored, first.String())
	}
	sessionAfterFirst := m.srtpSession

	// Second companion device accepts the same offer.
	second := types.JID{User: "peer", Device: 2, Server: types.HiddenUserServer}
	m.HandleCallAccept(context.Background(), acceptNode(callID), second)

	if m.acceptedByJid != anchored {
		t.Fatalf("a later accept moved the SRTP peer: got %q want %q; audio from the device that answered would start failing auth",
			m.acceptedByJid, anchored)
	}
	if m.srtpSession != sessionAfterFirst {
		t.Fatal("a later accept replaced the live SRTP session")
	}
}

// The very first accept must still anchor normally: the guard is "do not move
// it", not "do not set it".
func TestFirstAcceptStillAnchorsThePeer(t *testing.T) {
	const callID = "CID"
	m := NewCallManager(&stubSocket{lid: types.JID{User: "me", Server: types.HiddenUserServer}}, slog.Default())
	call := NewOutgoingCall(callID, "peer@lid", "me@lid", core.CallMediaTypeAudio)
	call.EncryptionKey = make([]byte, 32)
	if err := call.ApplyTransition(Transition{Type: TransitionOfferSent}); err != nil {
		t.Fatalf("offer_sent: %v", err)
	}
	m.currentCall = call

	peer := types.JID{User: "peer", Device: 1, Server: types.HiddenUserServer}
	m.HandleCallAccept(context.Background(), acceptNode(callID), peer)

	if m.acceptedByJid != peer.String() {
		t.Fatalf("acceptedByJid = %q, want %q", m.acceptedByJid, peer.String())
	}
	if m.srtpSession == nil {
		t.Fatal("first accept should have initialised the SRTP session")
	}
}
