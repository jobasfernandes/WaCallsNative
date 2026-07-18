package session

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	pionmedia "github.com/pion/webrtc/v4/pkg/media"
)

// makeBrowserOffer simulates the browser: it opens the "pcm" data channel and
// returns the SDP offer. It also returns the peer connection so a test can
// finish the handshake and exchange PCM.
func makeBrowserOffer(t *testing.T) (*webrtc.PeerConnection, *webrtc.DataChannel, string) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}

	dc, err := pc.CreateDataChannel(pcmChannelLabel, nil)
	if err != nil {
		t.Fatal(err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gather
	return pc, dc, pc.LocalDescription().SDP
}

func TestNewBridgeNegotiatesDataChannelOffer(t *testing.T) {
	pc, _, offer := makeBrowserOffer(t)
	defer func() { _ = pc.Close() }()

	br, answer, err := NewBridge(webrtc.NewAPI(), offer, slog.Default())
	if err != nil {
		t.Fatalf("NewBridge failed: %v", err)
	}
	defer br.Close()

	if answer == "" || !strings.Contains(answer, "m=application") {
		t.Fatalf("answer missing data channel (application) m-line:\n%s", answer)
	}
	if strings.Contains(answer, "m=audio") {
		t.Fatalf("answer should not negotiate an audio m-line anymore:\n%s", answer)
	}
}

func videoCapableAPI(t *testing.T) *webrtc.API {
	t.Helper()
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(me))
}

// TestNewBridgeAddsVideoTrackWhenOfferHasVideo verifies the bridge answers with an H264 m=video
// line (and creates a video track) only when the browser offer carries m=video.
func TestNewBridgeAddsVideoTrackWhenOfferHasVideo(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	if _, err := pc.CreateDataChannel(pcmChannelLabel, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gather

	br, answer, err := NewBridge(videoCapableAPI(t), pc.LocalDescription().SDP, slog.Default())
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer br.Close()
	if br.videoTrack == nil {
		t.Fatal("bridge must create a video track when the offer has m=video")
	}
	if !strings.Contains(answer, "m=video") {
		t.Fatalf("answer missing m=video:\n%s", answer)
	}
	if !strings.Contains(answer, "H264") && !strings.Contains(answer, "h264") {
		t.Fatalf("answer m=video missing H264 codec:\n%s", answer)
	}
}

func TestNewBridgeNoVideoTrackForAudioOnlyOffer(t *testing.T) {
	pc, _, offer := makeBrowserOffer(t)
	defer func() { _ = pc.Close() }()
	br, answer, err := NewBridge(videoCapableAPI(t), offer, slog.Default())
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer br.Close()
	if br.videoTrack != nil {
		t.Fatal("audio-only offer must not create a video track")
	}
	if strings.Contains(answer, "m=video") {
		t.Fatalf("audio-only answer must not carry m=video:\n%s", answer)
	}
}

// TestBridgeReceivesBrowserVideo simulates a browser sending camera H264 over a sendrecv
// transceiver and asserts the bridge surfaces the RTP payloads via OnBrowserVideo.
func TestBridgeReceivesBrowserVideo(t *testing.T) {
	browser, err := videoCapableAPI(t).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = browser.Close() }()
	if _, err := browser.CreateDataChannel(pcmChannelLabel, nil); err != nil {
		t.Fatal(err)
	}
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "browser")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := browser.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	offer, err := browser.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gather := webrtc.GatheringCompletePromise(browser)
	if err := browser.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gather

	got := make(chan int, 1)
	br, answer, err := NewBridge(videoCapableAPI(t), browser.LocalDescription().SDP, slog.Default())
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer br.Close()
	br.OnBrowserVideo = func(payload []byte, _ uint32, _ bool) {
		select {
		case got <- len(payload):
		default:
		}
	}
	if err := browser.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatalf("browser SetRemoteDescription: %v", err)
	}

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		au := []byte{0x67, 0x42, 0x00, 0x0a, 0x00, 0x00, 0x00, 0x01, 0x65, 0x01, 0x02, 0x03}
		for {
			select {
			case <-stop:
				return
			default:
				_ = track.WriteSample(pionmedia.Sample{Data: au, Duration: 33 * time.Millisecond})
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()

	select {
	case <-got:
	case <-time.After(15 * time.Second):
		t.Fatal("OnBrowserVideo never fired")
	}
}

// TestBridgePCMRoundtrip connects the simulated browser to the bridge and checks
// that PCM sent on the data channel surfaces as float32 via OnBrowserPCM.
func TestBridgePCMRoundtrip(t *testing.T) {
	pc, dc, offer := makeBrowserOffer(t)
	defer func() { _ = pc.Close() }()

	got := make(chan []float32, 1)
	br, answer, err := NewBridge(webrtc.NewAPI(), offer, slog.Default())
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer br.Close()
	br.OnBrowserPCM = func(pcm []float32) { got <- pcm }

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatalf("browser SetRemoteDescription: %v", err)
	}

	dc.OnOpen(func() {
		// 0.5 and -0.5 in Int16 LE.
		_ = dc.Send([]byte{0x00, 0x40, 0x00, 0xC0})
	})

	select {
	case pcm := <-got:
		if len(pcm) != 2 {
			t.Fatalf("expected 2 samples, got %d", len(pcm))
		}
		if pcm[0] < 0.4 || pcm[0] > 0.6 || pcm[1] > -0.4 || pcm[1] < -0.6 {
			t.Fatalf("unexpected samples: %v", pcm)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for PCM over data channel")
	}
}
