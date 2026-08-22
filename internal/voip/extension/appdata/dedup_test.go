package appdata

import "testing"

func TestDedupAcceptsOnlyTheFirstCopy(t *testing.T) {
	d := newDedup()
	accepted := 0
	// O remetente retransmite a mesma reaction 10 vezes com o mesmo id.
	for range 10 {
		if d.accept(1) {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d copies, want exactly 1", accepted)
	}
}

func TestDedupAcceptsIncreasingIDs(t *testing.T) {
	d := newDedup()
	for _, id := range []uint64{1, 2, 3} {
		if !d.accept(id) {
			t.Fatalf("id %d must be accepted", id)
		}
	}
}

func TestDedupIgnoresReorderedOldID(t *testing.T) {
	d := newDedup()
	d.accept(5)
	if d.accept(3) {
		t.Fatal("an id below the high-water mark must be ignored")
	}
}

// Sem reset, um peer que reinicia a midia volta a contar de 1 e todas as
// reactions novas cairiam abaixo da marca, sumindo em silencio.
func TestDedupResetAcceptsLowIDAgain(t *testing.T) {
	d := newDedup()
	d.accept(100)
	d.reset()
	if !d.accept(1) {
		t.Fatal("after reset a low id must be accepted again")
	}
}
