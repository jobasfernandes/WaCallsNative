package call

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"wacalls/internal/voip/core"
)

// The host callback must never run while the manager lock is held: consumers do
// real work on it (the reference server closes a WebRTC peer connection on the
// ended branch), and a consumer that calls back into the manager would deadlock.
func TestStateCallbackDoesNotRunUnderTheManagerLock(t *testing.T) {
	m := &CallManager{log: slog.Default()}
	locked := make(chan bool, 1)

	m.OnStateChange = func(*CallInfo) {
		// TryLock succeeds only if nobody is holding m.mu right now.
		if m.mu.TryLock() {
			m.mu.Unlock()
			locked <- false
			return
		}
		locked <- true
	}

	call := NewOutgoingCall("CID", "peer@lid", "me@lid", core.CallMediaTypeAudio)
	m.mu.Lock()
	m.currentCall = call
	m.emitState()
	m.mu.Unlock()

	select {
	case heldByUs := <-locked:
		if heldByUs {
			t.Fatal("OnStateChange ran while the manager lock was held")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnStateChange never ran")
	}
}

// A consumer calling back into the manager is legitimate (the reference server
// removes the call from its registry on the ended branch) and must not deadlock.
func TestStateCallbackCanReenterTheManager(t *testing.T) {
	m := &CallManager{log: slog.Default()}
	done := make(chan struct{})

	m.OnStateChange = func(*CallInfo) {
		_ = m.CurrentCall() // takes m.mu
		close(done)
	}

	call := NewOutgoingCall("CID", "peer@lid", "me@lid", core.CallMediaTypeAudio)
	m.mu.Lock()
	m.currentCall = call
	m.emitState()
	m.mu.Unlock()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock: a consumer that reenters the manager never returned")
	}
}

// Order is semantic: a consumer must never see an earlier state after a later
// one. This is why the queue is drained by a single goroutine instead of one
// goroutine per event.
func TestQueuedStatesAreDeliveredInOrder(t *testing.T) {
	m := &CallManager{log: slog.Default()}

	var mu sync.Mutex
	var seen []core.CallState
	done := make(chan struct{})

	m.OnStateChange = func(c *CallInfo) {
		mu.Lock()
		seen = append(seen, c.StateData.State)
		n := len(seen)
		mu.Unlock()
		if n == 3 {
			close(done)
		}
	}

	call := NewOutgoingCall("CID", "peer@lid", "me@lid", core.CallMediaTypeAudio)
	m.mu.Lock()
	m.currentCall = call
	_ = call.ApplyTransition(Transition{Type: TransitionOfferSent})
	m.emitState()
	_ = call.ApplyTransition(Transition{Type: TransitionRemoteAccepted})
	m.emitState()
	_ = call.ApplyTransition(Transition{Type: TransitionMediaConnected})
	m.emitState()
	m.mu.Unlock()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("not all states were delivered")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []core.CallState{core.CallStateRinging, core.CallStateConnecting, core.CallStateActive}
	for i, w := range want {
		if seen[i] != w {
			t.Fatalf("out of order: got %v, want %v", seen, want)
		}
	}
}

// The snapshot must be immune to later mutation of the live call, or the
// consumer reads a struct the manager is still writing to.
func TestSnapshotIsIsolatedFromLaterMutation(t *testing.T) {
	m := &CallManager{log: slog.Default()}
	got := make(chan *CallInfo, 1)
	m.OnStateChange = func(c *CallInfo) { got <- c }

	call := NewOutgoingCall("CID", "peer@lid", "me@lid", core.CallMediaTypeAudio)
	m.mu.Lock()
	m.currentCall = call
	_ = call.ApplyTransition(Transition{Type: TransitionOfferSent})
	m.emitState()
	m.mu.Unlock()

	var snap *CallInfo
	select {
	case snap = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("OnStateChange never ran")
	}

	m.mu.Lock()
	_ = call.ApplyTransition(Transition{Type: TransitionRemoteAccepted})
	m.mu.Unlock()

	if snap.StateData.State != core.CallStateRinging {
		t.Fatalf("snapshot followed the live call: got %v, want %v",
			snap.StateData.State, core.CallStateRinging)
	}
}
