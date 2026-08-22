package appdata

import "sync"

// dedup keeps a high-water mark of the peer's transaction id. The sender
// retransmits each reaction ten times, so only the first copy that authenticates
// may reach the callback; the same rule drops ids that arrive out of order.
type dedup struct {
	mu   sync.Mutex
	last uint64
}

func newDedup() *dedup { return &dedup{} }

func (d *dedup) accept(transactionID uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if transactionID <= d.last {
		return false
	}
	d.last = transactionID
	return true
}

// reset must run whenever the media session restarts, because the peer's sender
// starts counting from one again and every new reaction would otherwise fall
// below the mark and be dropped without a trace.
func (d *dedup) reset() {
	d.mu.Lock()
	d.last = 0
	d.mu.Unlock()
}
