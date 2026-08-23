package audio

import (
	"context"
	"runtime/pprof"
	"sync"
	"time"

	"wacalls/internal/voip/codec/mlow"
	"wacalls/internal/voip/core"
	"wacalls/internal/voip/engine"
	"wacalls/internal/voip/media"
)

const audioCodecBytes = 96 * 1024

// jitterDepth is how many newer packets must pile up behind a missing one before it
// is concealed, giving a reordered packet time to arrive. 3 packets at 60 ms is 180 ms.
const jitterDepth = 3

type Audio struct {
	codec        core.AudioCodec
	scope        *engine.CallScope
	mu           sync.Mutex
	captureBuf   []float32
	sendLoopStop chan struct{}
	onPeerPCM    func([]float32)
	detached     bool

	// streams is one receive pipeline per sender SSRC. A 1:1 call has exactly
	// one; a group call has one per participant device.
	streams    map[uint32]*inboundStream
	newDecoder DecoderFactory
}

// DecoderFactory builds a decoder for a newly seen sender. It is nil on a 1:1
// call, where the single codec passed to New answers for the only stream.
type DecoderFactory func() (core.AudioCodec, error)

func New(codec core.AudioCodec) *Audio {
	return &Audio{codec: codec, streams: map[uint32]*inboundStream{}}
}

// SetDecoderFactory enables per-participant decoding. Without it a second sender
// is dropped, which is correct on a 1:1 call.
func (a *Audio) SetDecoderFactory(f DecoderFactory) {
	a.mu.Lock()
	a.newDecoder = f
	a.mu.Unlock()
}

func (a *Audio) Name() string {
	return "audio"
}

func (a *Audio) Attach(scope *engine.CallScope) error {
	a.mu.Lock()
	a.scope = scope
	scope.Observer.AddMem(audioCodecBytes)
	a.startSendLoopLocked()
	a.mu.Unlock()
	scope.OnRTP(core.PayloadTypeWhatsAppOpus, a.handleInbound)
	return nil
}

func (a *Audio) Detach() {
	a.mu.Lock()
	if a.detached {
		a.mu.Unlock()
		return
	}
	a.detached = true
	if a.sendLoopStop != nil {
		close(a.sendLoopStop)
		a.sendLoopStop = nil
	}
	scope := a.scope
	a.mu.Unlock()
	if scope != nil {
		scope.Observer.ReleaseMem(audioCodecBytes)
		a.logOffPointCounts(scope)
	}
	a.closeStreams()
	a.codec.Close()
}

// closeStreams releases the decoders the factory built. The first stream shares
// the extension's own codec, which Detach closes once on its own; closing it here
// too would free the native encoder state twice.
func (a *Audio) closeStreams() {
	a.mu.Lock()
	streams := a.streams
	a.streams = map[uint32]*inboundStream{}
	own := a.codec
	a.mu.Unlock()
	for _, s := range streams {
		if s.codec != own {
			s.close()
		}
	}
}

// logOffPointCounts reports, once per call, how many inbound frames the decoder
// silenced and why. It exists to answer empirically whether operating points the
// decoder does not implement (low_rate above all) actually reach us in the field:
// "inactive" is expected DTX and dominates the count, so the reasons must stay
// separate for the number to mean anything.
func (a *Audio) logOffPointCounts(scope *engine.CallScope) {
	counter, ok := a.codec.(core.OffPointCounter)
	if !ok {
		return
	}
	counts := counter.OffPointCounts()
	if len(counts) == 0 {
		return
	}
	attrs := []any{"call_id", scope.CallID}
	unimplemented := 0
	for _, reason := range []string{
		mlow.OffPointInactive, mlow.OffPointStdOpus,
		mlow.OffPointLowRate, mlow.OffPointSampleRate, mlow.OffPointFrameMs,
	} {
		n := counts[reason]
		attrs = append(attrs, reason, n)
		if reason != mlow.OffPointInactive && reason != mlow.OffPointStdOpus {
			unimplemented += n
		}
	}
	// An unimplemented operating point means real audio was replaced by silence,
	// which is the case worth acting on; DTX and routed standard Opus are not.
	if unimplemented > 0 {
		scope.Log.Warn("inbound frames silenced on an unimplemented operating point", attrs...)
		return
	}
	scope.Log.Info("inbound frames silenced", attrs...)
}

func (a *Audio) FeedPCM(pcm []float32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.codec == nil || len(pcm) == 0 {
		return
	}
	a.captureBuf = append(a.captureBuf, pcm...)
	if maxBuffered := a.codec.FrameSize() * 4; len(a.captureBuf) > maxBuffered {
		a.captureBuf = a.captureBuf[len(a.captureBuf)-maxBuffered:]
	}
}

func (a *Audio) OnPeerPCM(cb func([]float32)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onPeerPCM = cb
}

func (a *Audio) startSendLoopLocked() {
	if a.sendLoopStop != nil || a.codec == nil {
		return
	}
	stop := make(chan struct{})
	a.sendLoopStop = stop
	frameSize := a.codec.FrameSize()
	done := a.scope.Observer.TrackGoroutine()
	callID := a.scope.CallID
	go func() {
		pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(), pprof.Labels("call_id", callID)))
		defer done()
		ticker := time.NewTicker(60 * time.Millisecond)
		defer ticker.Stop()
		silence := make([]float32, frameSize)
		voiced := make([]float32, frameSize)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			a.mu.Lock()
			if a.scope == nil || a.codec == nil || a.scope.Relay == nil || !a.scope.Relay.HasConnection() {
				a.mu.Unlock()
				continue
			}
			frame := silence
			if len(a.captureBuf) >= frameSize {
				copy(voiced, a.captureBuf[:frameSize])
				frame = voiced
				a.captureBuf = a.captureBuf[frameSize:]
			}
			scope := a.scope
			codec := a.codec
			a.mu.Unlock()
			if enc, err := codec.Encode(frame); err == nil {
				_ = scope.SendAudioFrame(enc, codec.FrameSize())
			}
		}
	}()
}

// handleInbound routes the packet to its sender's pipeline and emits whatever the
// mix releases. Routing by SSRC is what lets a group call decode each participant
// with its own stateful decoder.
func (a *Audio) handleInbound(pkt *media.RtpPacket) {
	a.mu.Lock()
	stream := a.streamFor(pkt.Header.Ssrc)
	if stream == nil {
		a.mu.Unlock()
		return
	}
	stream.push(pkt.Header.SequenceNumber, pkt.Payload)
	out := a.drainMixLocked()
	cb := a.onPeerPCM
	a.mu.Unlock()
	if cb != nil {
		for _, pcm := range out {
			cb(pcm)
		}
	}
}

// streamFor returns the pipeline for one sender, creating it on first sight. The
// first sender uses the codec this extension was built with; any further sender
// needs the decoder factory, because one decoder cannot serve two streams.
// Caller holds a.mu.
func (a *Audio) streamFor(ssrc uint32) *inboundStream {
	if s, ok := a.streams[ssrc]; ok {
		return s
	}
	if len(a.streams) == 0 {
		s := newInboundStream(a.codec)
		a.streams[ssrc] = s
		return s
	}
	if a.newDecoder == nil {
		if a.scope != nil {
			a.scope.Log.Debug("dropping audio from a second sender: no decoder factory",
				"ssrc", ssrc)
		}
		return nil
	}
	decoder, err := a.newDecoder()
	if err != nil {
		if a.scope != nil {
			a.scope.Log.Warn("participant decoder unavailable", "ssrc", ssrc, "err", err)
		}
		return nil
	}
	s := newInboundStream(decoder)
	a.streams[ssrc] = s
	return s
}

// drainMixLocked emits one summed frame per round while every active stream has a
// frame ready. A stream that stops delivering is skipped once another has run
// maxMixLag frames ahead, so one silent participant cannot hold up the others.
// Caller holds a.mu.
func (a *Audio) drainMixLocked() [][]float32 {
	var out [][]float32
	for {
		contributors, ok := a.readyContributorsLocked()
		if !ok {
			return out
		}
		out = append(out, mixFrames(contributors))
	}
}

// readyContributorsLocked takes one frame from each stream that should be in the
// next mixed frame, or reports that the mix must wait. Caller holds a.mu.
func (a *Audio) readyContributorsLocked() ([][]float32, bool) {
	if len(a.streams) == 0 {
		return nil, false
	}
	maxReady := 0
	for _, s := range a.streams {
		if len(s.ready) > maxReady {
			maxReady = len(s.ready)
		}
	}
	if maxReady == 0 {
		return nil, false
	}
	// Wait for the stragglers only while nobody has run too far ahead.
	if maxReady < maxMixLag {
		for _, s := range a.streams {
			if len(s.ready) == 0 {
				return nil, false
			}
		}
	}
	var contributors [][]float32
	for _, s := range a.streams {
		if len(s.ready) == 0 {
			continue
		}
		contributors = append(contributors, s.ready[0])
		s.ready = s.ready[1:]
	}
	if len(contributors) == 0 {
		return nil, false
	}
	return contributors, true
}

// mixFrames sums the contributors sample by sample, saturating at the edges of
// the range rather than wrapping.
func mixFrames(frames [][]float32) []float32 {
	if len(frames) == 1 {
		return frames[0]
	}
	size := 0
	for _, f := range frames {
		if len(f) > size {
			size = len(f)
		}
	}
	out := make([]float32, size)
	for _, f := range frames {
		for i, v := range f {
			out[i] += v
		}
	}
	for i, v := range out {
		if v > 1 {
			out[i] = 1
		} else if v < -1 {
			out[i] = -1
		}
	}
	return out
}

var _ core.AudioSink = (*Audio)(nil)
var _ engine.Extension = (*Audio)(nil)
