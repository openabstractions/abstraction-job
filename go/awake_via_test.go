package job

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

type answers struct {
	err   error
	taken chan struct{}
	asked int
}

func (a *answers) Hold(who, why string) (Held, error) {
	a.asked++
	if a.err != nil {
		return nil, a.err
	}
	return held{free: func() {}, done: a.taken}, nil
}

func TestARefusalHoldsNothingAndSaysWhy(t *testing.T) {
	s := NewMemoryStore()
	refused := errors.New("rights: not granted")
	by := &answers{err: refused}
	h := KeepAwakeVia(by, s, claimed(t, s, time.Minute))
	defer h.Release()
	if by.asked != 1 {
		t.Fatalf("asked %d times", by.asked)
	}
	if h.Held() || !errors.Is(h.Why(), refused) {
		t.Fatalf("refused: held=%v, why=%v", h.Held(), h.Why())
	}
}

func TestNobodyAnsweringMeansThePlatform(t *testing.T) {
	s := NewMemoryStore()
	by := &answers{err: fmt.Errorf("%w: rights: no service", ErrAbsent)}
	h := KeepAwakeVia(by, s, claimed(t, s, time.Minute))
	defer h.Release()
	if by.asked != 1 {
		t.Fatalf("asked %d times", by.asked)
	}
	switch refused := CanKeepAwake(); {
	case refused == nil && (!h.Held() || h.Why() != nil):
		t.Fatalf("this machine can be kept awake: held=%v, why=%v", h.Held(), h.Why())
	case refused != nil && (h.Held() || h.Why() == nil || errors.Is(h.Why(), ErrAbsent)):
		t.Fatalf("this machine refuses to be kept awake (%v): held=%v, why=%v", refused, h.Held(), h.Why())
	}
}

func TestAHoldTakenAwayEndsAndDoesNotFallBack(t *testing.T) {
	s := NewMemoryStore()
	by := &answers{taken: make(chan struct{})}
	h := KeepAwakeVia(by, s, claimed(t, s, time.Minute))
	if !h.Held() || h.Why() != nil {
		t.Fatalf("granted: held=%v, why=%v", h.Held(), h.Why())
	}
	close(by.taken)
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the hold did not notice it was taken away; its lease, a minute long, would have ended it instead")
	}
	if h.Held() || !errors.Is(h.Why(), ErrTakenAway) || by.asked != 1 {
		t.Fatalf("taken away: held=%v, why=%v, asked=%d", h.Held(), h.Why(), by.asked)
	}
	h.Release()
}
