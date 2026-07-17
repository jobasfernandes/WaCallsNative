package session

import (
	"context"
	"errors"
	"testing"
)

func TestVideoActionRejectsBadAction(t *testing.T) {
	m := newTestManager(t)
	s := m.addUnconnected(t, "acct")

	if err := s.VideoAction(context.Background(), "anycall", "dance", nil); !errors.Is(err, ErrBadVideoAction) {
		t.Fatalf("bad action must return ErrBadVideoAction, got %v", err)
	}
}

func TestVideoActionRejectsBadOrientation(t *testing.T) {
	m := newTestManager(t)
	s := m.addUnconnected(t, "acct")

	bad := 7
	if err := s.VideoAction(context.Background(), "anycall", "orientation", &bad); !errors.Is(err, ErrBadVideoAction) {
		t.Fatalf("orientation 7 must return ErrBadVideoAction, got %v", err)
	}
	if err := s.VideoAction(context.Background(), "anycall", "orientation", nil); !errors.Is(err, ErrBadVideoAction) {
		t.Fatalf("orientation without value must return ErrBadVideoAction, got %v", err)
	}
}

func TestVideoActionUnknownCallReturnsError(t *testing.T) {
	m := newTestManager(t)
	s := m.addUnconnected(t, "acct")

	if err := s.VideoAction(context.Background(), "ghost", "request", nil); err == nil {
		t.Fatal("request on an unknown call must return an error")
	}
}
