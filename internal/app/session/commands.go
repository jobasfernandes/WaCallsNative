package session

import (
	"context"
	"errors"
	"fmt"

	"wacalls/internal/voip/call"
	"wacalls/internal/voip/core"

	"go.mau.fi/whatsmeow/types"
)

var ErrTooManyCalls = errors.New("max concurrent calls")

var ErrBadVideoAction = errors.New("invalid video action")

type StartedCall struct{ CallID, Peer, PeerName, PeerPhotoURL string }

func (s *Session) ID() string { return s.id }

func (s *Session) IsPaired() bool { return s.client.Store.ID != nil }

func (s *Session) HasCall(callID string) bool {
	_, ok := s.calls.Get(callID)
	return ok
}

func (s *Session) StartCall(ctx context.Context, phone string, video bool) (StartedCall, error) {
	if max := s.mgr.maxCalls; max > 0 && s.calls.Count() >= max {
		return StartedCall{}, ErrTooManyCalls
	}
	peer := types.NewJID(phone, types.DefaultUserServer)
	callID, err := s.calls.StartCall(ctx, peer, video)
	if err != nil {
		return StartedCall{}, err
	}
	return StartedCall{
		CallID:       callID,
		Peer:         peer.String(),
		PeerName:     resolvePeerName(ctx, s.client, peer),
		PeerPhotoURL: cachedPhotoURL(ctx, s.mgr.photos, s.id, peer.String()),
	}, nil
}

func (s *Session) FetchCallPhoto(callID, peer string) {
	jid, err := types.ParseJID(peer)
	if err != nil {
		return
	}
	go s.fetchPeerPhoto(jid, callID)
}

func (s *Session) AcceptCall(ctx context.Context, callID string) error {
	return s.calls.AcceptCall(ctx, callID)
}

func (s *Session) RejectCall(ctx context.Context, callID string) error {
	err := s.calls.RejectCall(ctx, callID, core.EndCallReasonDeclined)
	var invalid *call.InvalidTransition
	if errors.As(err, &invalid) {
		return err
	}
	s.removeCall(callID)
	return nil
}

func (s *Session) SetMute(ctx context.Context, callID string, muted bool) error {
	return s.calls.SetMute(ctx, callID, muted)
}

func (s *Session) VideoAction(ctx context.Context, callID, action string, orientation *int) error {
	switch action {
	case "request":
		return s.calls.RequestVideoUpgrade(ctx, callID)
	case "accept":
		return s.calls.AcceptVideoUpgrade(ctx, callID)
	case "reject":
		return s.calls.RejectVideoUpgrade(ctx, callID)
	case "stop":
		return s.calls.StopVideo(ctx, callID)
	case "orientation":
		if orientation == nil || *orientation < 0 || *orientation > 3 {
			return ErrBadVideoAction
		}
		return s.calls.SetVideoOrientation(ctx, callID, *orientation)
	default:
		return ErrBadVideoAction
	}
}

func (s *Session) EndCall(ctx context.Context, callID string) error {
	err := s.calls.EndCall(ctx, callID, core.EndCallReasonUserEnded)
	s.removeCall(callID)
	return err
}

func (s *Session) AttachBrowser(callID, offerSDP string) (string, error) {
	cm, ok := s.calls.Get(callID)
	if !ok {
		return "", fmt.Errorf("no such call %s", callID)
	}
	bridge, answer, err := NewBridge(s.mgr.webrtcAPI, offerSDP, s.log)
	if err != nil {
		return "", err
	}
	bridge.OnBrowserPCM = func(pcm []float32) { cm.FeedCapturedPCM(pcm) }
	bridge.OnTerminalICE = func() { go s.onBridgeDetached(callID, bridge) }
	s.setBridge(callID, bridge)
	return answer, nil
}
