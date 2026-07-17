package call

import (
	"context"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/signaling"
	"wacalls/internal/voip/wanode"

	waBinary "go.mau.fi/whatsmeow/binary"
)

func (m *CallManager) videoSnapshotLocked() core.VideoSnapshot {
	pending := ""
	switch {
	case m.videoGate:
		pending = "out"
	case m.videoPendingIn:
		pending = "in"
	}
	return core.VideoSnapshot{
		Local:       m.localVideo,
		Remote:      m.remoteVideo,
		Pending:     pending,
		Orientation: m.peerVideoOrientation,
	}
}

func (m *CallManager) emitVideoLocked(callID string) {
	if m.OnVideoState != nil {
		m.OnVideoState(callID, m.videoSnapshotLocked())
	}
}

// videoDestLocked routes in-call video signaling to the device that answered, like the
// mute and terminate paths (callmanager.go SetMute); falls back to the base peer jid.
func (m *CallManager) videoDestLocked(call *CallInfo) (dest, callID, creator string) {
	dest = call.PeerJid
	if m.acceptedByJid != "" {
		dest = m.acceptedByJid
	}
	return dest, call.CallID, call.CallCreator
}

func (m *CallManager) sendVideoState(ctx context.Context, dest, callID, creator string, state int, dec string, orientation *int) error {
	node := signaling.BuildVideoStateStanza(signaling.VideoStateParams{
		CallID: callID, To: wanode.MustJID(dest), CallCreator: wanode.MustJID(creator),
		State: state, Dec: dec, DeviceOrientation: orientation,
	})
	return m.sock.SendNode(ctx, node)
}

func (m *CallManager) RequestVideoUpgrade(ctx context.Context) error {
	m.mu.Lock()
	call := m.currentCall
	if call == nil || !call.IsActive() {
		m.mu.Unlock()
		return &CallError{"video upgrade needs a connected call"}
	}
	if m.localVideo {
		m.mu.Unlock()
		return &CallError{"video already requested or active"}
	}
	m.localVideo = true
	m.videoGate = true
	dest, callID, creator := m.videoDestLocked(call)
	m.emitVideoLocked(callID)
	m.mu.Unlock()

	if err := m.sendVideoState(ctx, dest, callID, creator, core.VideoStateUpgradeRequestV2, signaling.VideoDecRequest, nil); err != nil {
		m.mu.Lock()
		m.localVideo = false
		m.videoGate = false
		m.emitVideoLocked(callID)
		m.mu.Unlock()
		return err
	}
	m.log.Info("video upgrade requested", "call_id", callID)
	return nil
}

func (m *CallManager) AcceptVideoUpgrade(ctx context.Context) error {
	m.mu.Lock()
	call := m.currentCall
	if call == nil || call.IsEnded() || !m.videoPendingIn {
		m.mu.Unlock()
		return &CallError{"no pending video upgrade"}
	}
	m.videoPendingIn = false
	m.localVideo = true
	m.videoGate = false
	dest, callID, creator := m.videoDestLocked(call)
	m.emitVideoLocked(callID)
	m.mu.Unlock()

	if err := m.sendVideoState(ctx, dest, callID, creator, core.VideoStateUpgradeAccept, signaling.VideoDecAccept, nil); err != nil {
		m.mu.Lock()
		m.localVideo = false
		m.emitVideoLocked(callID)
		m.mu.Unlock()
		return err
	}
	return m.sendVideoState(ctx, dest, callID, creator, core.VideoStateEnabled, "", nil)
}

func (m *CallManager) RejectVideoUpgrade(ctx context.Context) error {
	m.mu.Lock()
	call := m.currentCall
	if call == nil || call.IsEnded() || !m.videoPendingIn {
		m.mu.Unlock()
		return &CallError{"no pending video upgrade"}
	}
	m.videoPendingIn = false
	dest, callID, creator := m.videoDestLocked(call)
	m.emitVideoLocked(callID)
	m.mu.Unlock()
	return m.sendVideoState(ctx, dest, callID, creator, core.VideoStateUpgradeReject, "", nil)
}

func (m *CallManager) StopVideo(ctx context.Context) error {
	m.mu.Lock()
	call := m.currentCall
	if call == nil || call.IsEnded() || !m.localVideo {
		m.mu.Unlock()
		return &CallError{"video is not active"}
	}
	m.localVideo = false
	m.videoGate = false
	dest, callID, creator := m.videoDestLocked(call)
	m.emitVideoLocked(callID)
	m.mu.Unlock()
	orientation := 0
	return m.sendVideoState(ctx, dest, callID, creator, core.VideoStateStopped, "", &orientation)
}

func (m *CallManager) SetVideoOrientation(ctx context.Context, orientation int) error {
	if orientation < 0 || orientation > 3 {
		return &CallError{"orientation outside 0..3"}
	}
	m.mu.Lock()
	call := m.currentCall
	if call == nil || call.IsEnded() || !m.localVideo {
		m.mu.Unlock()
		return &CallError{"video is not active"}
	}
	dest, callID, creator := m.videoDestLocked(call)
	m.mu.Unlock()
	return m.sendVideoState(ctx, dest, callID, creator, core.VideoStateEnabled, "", &orientation)
}

// HandleVideoStanza processes an inbound standalone <video> transition and updates the flow
// state machine. whatsmeow surfaces no event for it (like mute_v2), so it is intercepted.
func (m *CallManager) HandleVideoStanza(node *waBinary.Node) {
	info := signaling.ExtractNodeInfo(node)
	if info == nil || info.Tag != "video" {
		return
	}
	m.mu.Lock()
	call := m.currentCall
	if call == nil || call.CallID != info.CallID || call.IsEnded() {
		m.mu.Unlock()
		return
	}
	if m.acceptedByJid != "" && info.PeerJid != "" && ensureDeviceJid(info.PeerJid) != ensureDeviceJid(m.acceptedByJid) {
		m.mu.Unlock()
		m.log.Debug("video stanza from non-answering device ignored", "call_id", info.CallID, "from", info.PeerJid)
		return
	}
	state, orientation := signaling.ParseVideoState(info.InnerNode)
	announce := false
	fireRequest := false
	switch state {
	case core.VideoStateEnabled:
		m.remoteVideo = true
		m.peerVideoOrientation = orientation
		if m.localVideo && m.videoGate {
			m.videoGate = false
		}
	case core.VideoStateDisabled, core.VideoStateStopped:
		m.remoteVideo = false
	case core.VideoStateUpgradeAccept:
		m.localVideo = true
		m.videoGate = false
		announce = true
	case core.VideoStateUpgradeReject, core.VideoStateUpgradeCancel:
		m.localVideo = false
		m.videoGate = false
		m.videoPendingIn = false
	case core.VideoStateUpgradeRequest, core.VideoStateUpgradeRequestV2:
		if m.videoGate {
			// Glare: both sides requested video, treat as mutual accept.
			m.videoGate = false
			m.remoteVideo = true
			announce = true
		} else {
			m.videoPendingIn = true
			fireRequest = true
		}
	default:
		m.log.Debug("unhandled video state", "call_id", info.CallID, "state", state)
	}
	dest, callID, creator := m.videoDestLocked(call)
	m.emitVideoLocked(callID)
	m.mu.Unlock()

	m.log.Info("inbound video state", "call_id", callID, "state", state, "orientation", orientation)
	if announce {
		if err := m.sendVideoState(context.Background(), dest, callID, creator, core.VideoStateEnabled, "", nil); err != nil {
			m.mu.Lock()
			m.localVideo = false
			m.videoGate = false
			m.emitVideoLocked(callID)
			m.mu.Unlock()
			m.log.Warn("video enabled announcement failed", "call_id", callID, "err", err)
		}
	}
	if fireRequest && m.OnVideoUpgradeRequest != nil {
		m.OnVideoUpgradeRequest(callID)
	}
}
