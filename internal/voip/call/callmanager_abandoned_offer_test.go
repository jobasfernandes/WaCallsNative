package call

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"

	"wacalls/internal/voip/core"
)

// noopSocket is a do-nothing core.VoipSocket. Only OwnLID matters here: the
// call manager reaches the socket through it while building the offer.
type noopSocket struct{ lid types.JID }

func (s *noopSocket) OwnPN() types.JID  { return types.JID{} }
func (s *noopSocket) OwnLID() types.JID { return s.lid }
func (s *noopSocket) AccountDeviceIdentityNode() (waBinary.Node, bool) {
	return waBinary.Node{}, false
}
func (s *noopSocket) SendNode(context.Context, waBinary.Node) error { return nil }
func (s *noopSocket) Query(context.Context, waBinary.Node) (*waBinary.Node, error) {
	return nil, nil
}
func (s *noopSocket) GetUSyncDevices(_ context.Context, jids []types.JID) ([]types.JID, error) {
	return jids, nil
}
func (s *noopSocket) AssertSessions(context.Context, []types.JID, bool) error { return nil }
func (s *noopSocket) CreateParticipantNodes(context.Context, []types.JID, []byte, waBinary.Attrs) ([]waBinary.Node, bool, error) {
	return nil, false, nil
}
func (s *noopSocket) DecryptCallKey(context.Context, types.JID, *waBinary.Node) ([]byte, error) {
	return nil, nil
}
func (s *noopSocket) GetTCToken(context.Context, types.JID) ([]byte, error)     { return nil, nil }
func (s *noopSocket) ResolveLIDForPN(_ context.Context, pn types.JID) types.JID { return pn }

var _ core.VoipSocket = (*noopSocket)(nil)

// offerSocket records the stanzas sent through it and lets a test decide what
// the offer Query reports: whether the node reached the wire, and which error
// comes back.
type offerSocket struct {
	noopSocket

	wrote    bool
	queryErr error

	mu   sync.Mutex
	sent []waBinary.Node
	seen chan struct{}
}

func newOfferSocket(wrote bool, queryErr error) *offerSocket {
	return &offerSocket{
		noopSocket: noopSocket{lid: types.JID{User: "me", Server: types.HiddenUserServer}},
		wrote:      wrote,
		queryErr:   queryErr,
		seen:       make(chan struct{}, 4),
	}
}

func (s *offerSocket) QueryReportingWrite(_ context.Context, node waBinary.Node) (*waBinary.Node, bool, error) {
	s.record(node)
	return nil, s.wrote, s.queryErr
}

func (s *offerSocket) Query(_ context.Context, node waBinary.Node) (*waBinary.Node, error) {
	s.record(node)
	return nil, nil
}

func (s *offerSocket) record(node waBinary.Node) {
	s.mu.Lock()
	s.sent = append(s.sent, node)
	s.mu.Unlock()
	select {
	case s.seen <- struct{}{}:
	default:
	}
}

// awaitStanzas waits for n stanzas to be sent, or fails. The teardown runs in
// its own goroutine, so polling a counter under a lock is the honest way to
// observe it without a sleep.
func (s *offerSocket) awaitStanzas(t *testing.T, n int) []waBinary.Node {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		s.mu.Lock()
		got := len(s.sent)
		s.mu.Unlock()
		if got >= n {
			s.mu.Lock()
			out := append([]waBinary.Node(nil), s.sent...)
			s.mu.Unlock()
			return out
		}
		select {
		case <-s.seen:
		case <-deadline:
			t.Fatalf("expected %d stanza(s), saw %d", n, got)
		}
	}
}

func hasTerminate(nodes []waBinary.Node) bool {
	for _, n := range nodes {
		for _, child := range n.GetChildren() {
			if child.Tag == "terminate" {
				return true
			}
		}
	}
	return false
}

// An origination that fails after the offer reached the wire must tear the leg
// down: the callee's phone is already ringing, and returning only the error
// leaves a call nobody on our side can hang up.
func TestStartCallTerminatesAnOfferThatReachedTheWire(t *testing.T) {
	sock := newOfferSocket(true, context.DeadlineExceeded)
	m := NewCallManager(sock, slog.Default())

	err := m.StartCall(context.Background(), "CID",
		types.JID{User: "peer", Server: types.HiddenUserServer}, false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StartCall must return the original error, got %v", err)
	}

	sent := sock.awaitStanzas(t, 2) // offer + terminate
	if !hasTerminate(sent) {
		t.Fatal("no terminate was sent — the offer stays orphaned and the callee keeps ringing")
	}
}

// When the write itself failed nothing is ringing, so a terminate would be
// noise addressed to a call that never existed.
func TestStartCallSendsNoTerminateWhenTheOfferNeverLeft(t *testing.T) {
	sock := newOfferSocket(false, errors.New("connection reset"))
	m := NewCallManager(sock, slog.Default())

	if err := m.StartCall(context.Background(), "CID",
		types.JID{User: "peer", Server: types.HiddenUserServer}, false); err == nil {
		t.Fatal("StartCall should have returned the socket error")
	}

	// The teardown, when it happens, is sent from its own goroutine — so this
	// has to give it a window instead of reading the slice straight away, or it
	// would pass even if a spurious terminate were on its way.
	if sock.awaitNoFurtherStanza(200 * time.Millisecond) {
		t.Fatal("a terminate was sent for an offer that never reached the wire")
	}
}

// awaitNoFurtherStanza reports whether any stanza beyond the offer shows up
// within d. Used to assert an absence, which needs a window to be meaningful.
func (s *offerSocket) awaitNoFurtherStanza(d time.Duration) bool {
	deadline := time.After(d)
	for {
		s.mu.Lock()
		got := len(s.sent)
		s.mu.Unlock()
		if got > 1 {
			return true
		}
		select {
		case <-s.seen:
		case <-deadline:
			return false
		}
	}
}
