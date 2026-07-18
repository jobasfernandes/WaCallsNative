package session

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wacalls/internal/voip/media"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	pionmedia "github.com/pion/webrtc/v4/pkg/media"
)

// pcmChannelLabel is the data channel the browser opens to carry raw 16 kHz mono
// Int16 LE PCM in both directions. The browser side must create it with this label.
const pcmChannelLabel = "pcm"

// metaChannelLabel is a server->browser control data channel carrying the peer's video
// orientation as {"rot":<deg>} so the browser can render portrait video upright.
const metaChannelLabel = "meta"

// Bridge is the browser-leg adapter: it carries raw PCM between the browser and
// the CallManager over a WebRTC data channel. The call core only ever sees
// []float32 PCM, so it stays unaware of the transport (no Opus here anymore).
type Bridge struct {
	pc          *webrtc.PeerConnection
	dc          atomic.Pointer[webrtc.DataChannel]
	videoTrack  *webrtc.TrackLocalStaticSample
	meta        atomic.Pointer[webrtc.DataChannel]
	lastRotSent atomic.Int32
	log         *slog.Logger

	browserVideoSSRC atomic.Uint32
	lastPLI          atomic.Int64
	pumpStop         chan struct{}
	pumpOnce         sync.Once
	closeOnce        sync.Once

	// OnBrowserPCM is invoked with decoded 16 kHz mono PCM captured from the browser mic.
	OnBrowserPCM func(pcm []float32)
	// OnBrowserVideo is invoked with each H.264 RTP payload the browser sends from its camera.
	OnBrowserVideo func(payload []byte, ts uint32, marker bool)
	// OnTerminalICE fires when the peer connection fails or closes.
	OnTerminalICE func()
}

func NewBridge(api *webrtc.API, offerSDP string, log *slog.Logger) (*Bridge, string, error) {
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, "", err
	}
	br := &Bridge{pc: pc, log: log, pumpStop: make(chan struct{})}

	// The browser offers an m=video (recvonly) line only for a video call; add the H264
	// downlink track only then, else the answer would carry an m-line the offer lacks.
	if strings.Contains(offerSDP, "m=video") {
		track, terr := webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "wacalls")
		if terr != nil {
			_ = pc.Close()
			return nil, "", terr
		}
		sender, aerr := pc.AddTrack(track)
		if aerr != nil {
			_ = pc.Close()
			return nil, "", aerr
		}
		br.videoTrack = track
		// Drain the sender's inbound RTCP so pion's buffer never fills.
		go func() {
			buf := make([]byte, 1500)
			for {
				if _, _, rerr := sender.Read(buf); rerr != nil {
					return
				}
			}
		}()
	}

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case pcmChannelLabel:
			br.dc.Store(dc)
			dc.OnMessage(func(msg webrtc.DataChannelMessage) {
				if cb := br.OnBrowserPCM; cb != nil && len(msg.Data) > 0 {
					cb(media.PCMInt16LEToFloat32(msg.Data))
				}
			})
		case metaChannelLabel:
			br.meta.Store(dc)
		}
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeVideo {
			return
		}
		br.browserVideoSSRC.Store(uint32(track.SSRC()))
		br.startKeyframePump()
		go func() {
			for {
				pkt, _, rerr := track.ReadRTP()
				if rerr != nil {
					return
				}
				if cb := br.OnBrowserVideo; cb != nil && len(pkt.Payload) > 0 {
					cb(pkt.Payload, pkt.Timestamp, pkt.Marker)
				}
			}
		}()
	})

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Debug("browser ice state", "state", s.String())
		if s == webrtc.ICEConnectionStateFailed || s == webrtc.ICEConnectionStateClosed {
			if br.OnTerminalICE != nil {
				br.OnTerminalICE()
			}
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		_ = pc.Close()
		return nil, "", err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		return nil, "", err
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		return nil, "", err
	}
	<-gatherComplete

	return br, pc.LocalDescription().SDP, nil
}

// WritePCM sends 16 kHz mono float32 PCM to the browser as Int16 LE over the data
// channel. It is a no-op until the channel is open.
func (b *Bridge) WritePCM(pcm []float32) error {
	dc := b.dc.Load()
	if dc == nil || len(pcm) == 0 {
		return nil
	}
	return dc.Send(media.PCMFloat32ToInt16LE(pcm))
}

// WriteVideo forwards one H.264 Annex-B access unit to the browser video track. The duration
// paces the track's RTP timestamps. No-op until a video track exists (audio-only call).
func (b *Bridge) WriteVideo(annexB []byte, dur time.Duration) error {
	if b.videoTrack == nil || len(annexB) == 0 {
		return nil
	}
	return b.videoTrack.WriteSample(pionmedia.Sample{Data: annexB, Duration: dur})
}

// SendRotation tells the browser the peer's video orientation (degrees) over the meta channel,
// so it can render portrait video upright. No-op until the channel opens; deduplicated.
func (b *Bridge) SendRotation(deg int) {
	dc := b.meta.Load()
	if dc == nil {
		return
	}
	if b.lastRotSent.Swap(int32(deg)+1) == int32(deg)+1 {
		return
	}
	_ = dc.SendText(fmt.Sprintf(`{"rot":%d}`, deg))
}

// RequestKeyframe asks the browser encoder for an IDR via an RTCP PLI, rate-limited to 300 ms.
func (b *Bridge) RequestKeyframe() {
	ssrc := b.browserVideoSSRC.Load()
	if ssrc == 0 {
		return
	}
	now := time.Now().UnixMilli()
	if now-b.lastPLI.Load() < 300 {
		return
	}
	b.lastPLI.Store(now)
	_ = b.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
}

// startKeyframePump drives a 1 s PLI to the browser once its camera video arrives: the WhatsApp
// peer never sends PLI, so periodic keyframes are the only resync path for the peer decoder.
func (b *Bridge) startKeyframePump() {
	b.pumpOnce.Do(func() {
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			b.RequestKeyframe()
			for {
				select {
				case <-b.pumpStop:
					return
				case <-t.C:
					b.RequestKeyframe()
				}
			}
		}()
	})
}

func (b *Bridge) Close() {
	b.closeOnce.Do(func() { close(b.pumpStop) })
	if b.pc != nil {
		_ = b.pc.Close()
	}
}
