package appdata

import (
	"context"
	"errors"
	"runtime/pprof"
	"sync"
	"time"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/engine"
	"wacalls/internal/voip/media"
)

const (
	// The sender repeats each reaction so a single lost packet costs nothing:
	// any one copy carries the whole reaction. Values match captured traffic.
	retransmitCount = 10
	retransmitDelay = 50 * time.Millisecond
	// timestampStep is synthetic: the app-data timestamp tracks no media clock,
	// so it neither matches the 16 kHz audio rate nor the 90 kHz video rate.
	timestampStep = 50
)

var errNotAttached = errors.New("appdata: no active media")

type AppData struct {
	mu       sync.Mutex
	scope    *engine.CallScope
	selfSsrc uint32
	seq      uint16
	ts       uint32
	txID     uint64
	stop     chan struct{}
	detached bool

	dedup  *dedup
	onPeer func(string)
}

func New() *AppData { return &AppData{dedup: newDedup()} }

func (a *AppData) Name() string { return "appdata" }

func (a *AppData) Attach(scope *engine.CallScope) error {
	a.mu.Lock()
	a.scope = scope
	a.detached = false
	a.stop = make(chan struct{})
	a.selfSsrc = media.GenerateSecureSsrc(scope.CallID, scope.OwnDeviceJID, core.SsrcCounterAppData)
	ssrc := a.selfSsrc
	a.mu.Unlock()

	// Without declaring it, the relay echo of our own stream comes back through
	// the RX dispatch and we would read our own reaction as the peer's.
	scope.DeclareSelfSSRC(ssrc)
	scope.OnRTP(core.PayloadTypeWhatsAppAppData, a.handleInbound)
	return nil
}

func (a *AppData) Detach() {
	a.mu.Lock()
	if a.detached {
		a.mu.Unlock()
		return
	}
	a.detached = true
	if a.stop != nil {
		close(a.stop)
		a.stop = nil
	}
	a.mu.Unlock()
}

func (a *AppData) OnPeerReaction(handler func(emoji string)) {
	a.mu.Lock()
	a.onPeer = handler
	a.mu.Unlock()
}

// ResetPeerState drops the dedup high-water mark. The call manager calls it when
// the media session restarts, because the peer's counter restarts too.
func (a *AppData) ResetPeerState() { a.dedup.reset() }

// AcceptsTransactionID reports whether an inbound id would still be accepted.
// Test seam for the reset coupling: a stale high-water mark silently swallows
// every reaction after the peer restarts its media session.
func (a *AppData) AcceptsTransactionID(id uint64) bool { return a.dedup.accept(id) }

func (a *AppData) SendReaction(emoji string) error {
	a.mu.Lock()
	if a.detached || a.scope == nil || a.scope.SendRTP == nil {
		a.mu.Unlock()
		return errNotAttached
	}
	a.txID++
	payload := encodeReaction(a.txID, emoji)
	scope, stop := a.scope, a.stop
	a.mu.Unlock()

	// The first copy goes out synchronously so an immediate transport failure
	// still reaches the caller; the rest would otherwise take ~450 ms.
	if err := a.sendOne(scope, payload); err != nil {
		return err
	}
	go a.retransmit(scope, stop, payload)
	return nil
}

func (a *AppData) sendOne(scope *engine.CallScope, payload []byte) error {
	a.mu.Lock()
	if a.detached {
		a.mu.Unlock()
		return errNotAttached
	}
	a.seq++
	a.ts += timestampStep
	header := media.NewRtpHeader(core.PayloadTypeWhatsAppAppData, a.seq, a.ts, a.selfSsrc)
	a.mu.Unlock()
	return scope.SendRTP(&media.RtpPacket{Header: header, Payload: payload})
}

func (a *AppData) retransmit(scope *engine.CallScope, stop chan struct{}, payload []byte) {
	done := scope.Observer.TrackGoroutine()
	defer done()
	pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(), pprof.Labels("call_id", scope.CallID)))
	ticker := time.NewTicker(retransmitDelay)
	defer ticker.Stop()
	for range retransmitCount - 1 {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		if err := a.sendOne(scope, payload); err != nil {
			return
		}
	}
}

func (a *AppData) handleInbound(pkt *media.RtpPacket) {
	reactions, err := decodeReactions(pkt.Payload)
	if err != nil {
		a.mu.Lock()
		scope := a.scope
		a.mu.Unlock()
		if scope != nil {
			scope.Log.Debug("appdata: dropping malformed payload", "ssrc", pkt.Header.Ssrc, "err", err)
		}
		return
	}
	a.mu.Lock()
	cb := a.onPeer
	a.mu.Unlock()
	if cb == nil {
		return
	}
	for _, r := range reactions {
		if a.dedup.accept(r.transactionID) {
			cb(r.emoji)
		}
	}
}

var _ core.ReactionSink = (*AppData)(nil)
var _ engine.Extension = (*AppData)(nil)
