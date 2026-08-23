package signaling

import (
	"fmt"
	"strconv"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

// ScreenShareState identifies one independent screen-share transition.
type ScreenShareState int

const (
	ScreenShareStarted ScreenShareState = 1
	ScreenShareStopped ScreenShareState = 2
)

// ScreenShare is the parsed state of one participant's screen share.
type ScreenShare struct {
	State            ScreenShareState
	Version          uint32
	ScreenShareID    uint32
	HasScreenShareID bool
}

// BuildRaiseHand builds one persistent participant hand-state transition.
func BuildRaiseHand(callID string, to, creator types.JID, requestID string, raised bool) waBinary.Node {
	state := "0"
	if raised {
		state = "1"
	}
	return callWrapWithID(to, requestID, waBinary.Node{
		Tag: "user_action",
		Attrs: waBinary.Attrs{
			"call-id": callID, "call-creator": creator, "action": "raise_hand",
		},
		Content: []waBinary.Node{{Tag: "raise_hand", Attrs: waBinary.Attrs{"raise-hand-state": state}}},
	})
}

// ParseRaiseHand parses one raise/lower action.
func ParseRaiseHand(node *waBinary.Node) (bool, error) {
	if node == nil || node.Tag != "user_action" {
		return false, fmt.Errorf("signaling: parse raise hand: unexpected node")
	}
	attrs := node.AttrGetter()
	if attrs.String("action") != "raise_hand" || attrs.String("call-id") == "" ||
		attrs.JID("call-creator").IsEmpty() {
		return false, fmt.Errorf("signaling: parse raise hand: invalid identity or action")
	}
	child, ok := node.GetOptionalChildByTag("raise_hand")
	if !ok {
		return false, fmt.Errorf("signaling: parse raise hand: missing state")
	}
	switch child.AttrGetter().String("raise-hand-state") {
	case "1":
		return true, nil
	case "0":
		return false, nil
	default:
		return false, fmt.Errorf("signaling: parse raise hand: invalid state")
	}
}

// BuildScreenShare builds one version-2 screen-share transition.
func BuildScreenShare(
	callID string,
	to, creator types.JID,
	requestID string,
	state ScreenShareState,
	screenShareID *uint32,
) waBinary.Node {
	attrs := waBinary.Attrs{
		"call-id": callID, "call-creator": creator,
		"screenshare_state": strconv.Itoa(int(state)), "version": "2",
	}
	if screenShareID != nil {
		attrs["screen_share_id"] = strconv.FormatUint(uint64(*screenShareID), 10)
	}
	return callWrapWithID(to, requestID, waBinary.Node{Tag: "screen_share", Attrs: attrs})
}

// ParseScreenShare parses one versioned screen-share transition.
func ParseScreenShare(node *waBinary.Node) (*ScreenShare, error) {
	if node == nil || node.Tag != "screen_share" {
		return nil, fmt.Errorf("signaling: parse screen share: unexpected node")
	}
	attrs := node.AttrGetter()
	stateValue, err := strconv.Atoi(attrs.String("screenshare_state"))
	if err != nil {
		return nil, fmt.Errorf("signaling: parse screen share state: %w", err)
	}
	state := ScreenShareState(stateValue)
	if state != ScreenShareStarted && state != ScreenShareStopped {
		return nil, fmt.Errorf("signaling: parse screen share: unsupported state %d", state)
	}
	version, err := strconv.ParseUint(attrs.String("version"), 10, 32)
	if err != nil {
		return nil, fmt.Errorf("signaling: parse screen share version: %w", err)
	}
	result := &ScreenShare{State: state, Version: uint32(version)}
	if rawID := attrs.OptionalString("screen_share_id"); rawID != "" {
		id, parseErr := strconv.ParseUint(rawID, 10, 32)
		if parseErr != nil {
			return nil, fmt.Errorf("signaling: parse screen share ID: %w", parseErr)
		}
		result.ScreenShareID = uint32(id)
		result.HasScreenShareID = true
	}
	if err = attrs.Error(); err != nil {
		return nil, fmt.Errorf("signaling: parse screen share: %w", err)
	}
	return result, nil
}
