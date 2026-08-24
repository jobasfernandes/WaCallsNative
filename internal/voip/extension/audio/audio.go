package audio

import (
	"context"
	"fmt"
	"runtime/pprof"
	"sort"
	"strings"
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
	framesIn     int
	framesMixed  int
	mixedOnce    bool
	lastEmit     time.Time
	firstEmit    time.Time
	samplesOut   int
	// now is the clock the output is paced against; tests replace it.
	now        func() time.Time
	emitGaps   []int
	burstSizes map[int]int
	detached   bool

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
	scope.OnRTP(core.PayloadTypeWhatsAppOpusAlt, a.handleInbound)
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
		a.mu.Lock()
		in, mixed, held := a.framesIn, a.framesMixed, a.heldFrames()
		a.mu.Unlock()
		failures, lastErr := a.decodeFailures()
		scope.Log.Info("inbound audio summary",
			"frames_in", in, "frames_mixed", mixed, "frames_held", held,
			"decode_failures", failures, "last_decode_err", lastErr,
			"dropped_late", a.droppedLate(),
			"cadence", a.cadenceSummary(),
			"output_rate", a.outputRate(),
			"toc_bytes", a.tocSummary(),
			"delivered", a.onPeerPCM != nil)
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
	stream.push(pkt.Header.SequenceNumber, pkt.Header.Timestamp,
		codecFrame(pkt.Header.PayloadType, pkt.Payload))
	a.framesIn++
	out := a.drainMixLocked()
	cb := a.onPeerPCM
	a.framesMixed += len(out)
	a.noteCadenceLocked(len(out))
	first := len(out) > 0 && !a.mixedOnce
	if first {
		a.mixedOnce = true
	}
	streams, scope := len(a.streams), a.scope
	a.mu.Unlock()
	if first && scope != nil {
		scope.Log.Info("first mixed frame emitted", "streams", streams)
	}
	if cb != nil {
		for _, pcm := range out {
			cb(pcm)
		}
	}
}

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
		frame := mixFrames(contributors)
		// The player consumes a fixed number of samples per second. Handing it
		// more fills its buffer until it overflows, and what it drops lands in
		// the middle of speech, so the surplus is dropped here instead: it is
		// the oldest audio, rather than whatever the player happens to evict.
		//
		// Holding it back instead was tried and is worse: the queue never
		// drains, latency grows without bound and most of the audio never
		// leaves at all.
		if a.outrunsRealTimeLocked(len(frame)) {
			continue
		}
		a.samplesOut += len(frame)
		out = append(out, frame)
	}
}

// playoutSlack is how far ahead of the clock the output may run. It covers the
// player's own start-up reserve plus normal arrival jitter, and no more: past
// that the surplus never gets played, it only sits in the player's buffer until
// something is evicted.
const playoutSlack = 0.4

// outrunsRealTimeLocked reports whether emitting this frame would put the output
// ahead of the clock.
//
// Caller holds a.mu.
func (a *Audio) outrunsRealTimeLocked(samples int) bool {
	if a.codec == nil {
		return false
	}
	clock := a.now
	if clock == nil {
		clock = time.Now
	}
	// The clock starts at the first frame that leaves; before that there is
	// nothing to be ahead of.
	if a.firstEmit.IsZero() {
		a.firstEmit = clock()
	}
	elapsed := clock().Sub(a.firstEmit).Seconds()
	allowed := (elapsed + playoutSlack) * float64(a.codec.SampleRate())
	return float64(a.samplesOut+samples) > allowed
}

// readyContributorsLocked takes one frame from each stream that should be in the
// next mixed frame, or reports that the mix must wait.
//
// Streams are paired by arrival, not by RTP timestamp: each sender starts its
// timestamp at an independent random value, so the timestamps of two
// participants cannot be compared. Correlating their clocks needs the RTCP
// sender reports, which this call does not receive.
//
// Caller holds a.mu.
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
		contributors = append(contributors, s.ready[0].pcm)
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

// heldFrames counts what the mix is still waiting on. A non-zero tail with zero
// mixed frames means the barrier never released.
func (a *Audio) heldFrames() int {
	n := 0
	for _, s := range a.streams {
		n += len(s.ready)
	}
	return n
}

// decodeFailures totals what the per-participant decoders rejected.
func (a *Audio) decodeFailures() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	var last error
	for _, s := range a.streams {
		n += s.decodeFailures
		if s.lastDecodeErr != nil {
			last = s.lastDecodeErr
		}
	}
	return n, last
}

// tocSummary renders the leading payload byte seen per stream.
func (a *Audio) tocSummary() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var parts []string
	for ssrc, s := range a.streams {
		parts = append(parts, fmt.Sprintf("ssrc=%d[%s %s]", ssrc, s.topTOCBytes(3), s.payloadShape()))
	}
	return strings.Join(parts, " ")
}

// codecFrame returns the bytes the MLow decoder expects, which start at the TOC.
//
// Group audio arrives under payload type 121 with one extra byte ahead of the
// TOC. Read without skipping it, every frame looks like a zeroed TOC: inactive,
// and therefore silenced, so the whole call plays as silence. A 1:1 stream
// carries the TOC in the first byte and needs no adjustment.
func codecFrame(payloadType uint8, payload []byte) []byte {
	if payloadType != core.PayloadTypeWhatsAppOpusAlt || len(payload) < 2 {
		return payload
	}
	return payload[1:]
}

// noteCadenceLocked records how the mix actually paces its output. Chopping is a
// pacing fault, and the gap between emissions plus how many frames leave at once
// is what separates a mix that stutters from audio that is simply missing.
//
// Caller holds a.mu.
func (a *Audio) noteCadenceLocked(emitted int) {
	if emitted == 0 {
		return
	}
	if a.burstSizes == nil {
		a.burstSizes = map[int]int{}
	}
	a.burstSizes[emitted]++
	now := time.Now()
	if !a.lastEmit.IsZero() {
		a.emitGaps = append(a.emitGaps, int(now.Sub(a.lastEmit).Milliseconds()))
	}
	a.lastEmit = now
}

// cadenceSummary reports the spread of the gaps between emissions. A 60 ms frame
// should leave roughly every 60 ms; long gaps followed by bursts are heard as
// chopping.
func (a *Audio) cadenceSummary() string {
	if len(a.emitGaps) == 0 {
		return "none"
	}
	sorted := append([]int(nil), a.emitGaps...)
	sort.Ints(sorted)
	over := 0
	for _, g := range sorted {
		if g > 100 {
			over++
		}
	}
	var bursts []string
	for size, n := range a.burstSizes {
		bursts = append(bursts, fmt.Sprintf("%dx%d", size, n))
	}
	sort.Strings(bursts)
	return fmt.Sprintf("p50=%dms p90=%dms max=%dms over100ms=%d/%d bursts=[%s]",
		sorted[len(sorted)/2], sorted[len(sorted)*9/10], sorted[len(sorted)-1],
		over, len(sorted), strings.Join(bursts, " "))
}

// outputRate compares how much audio left with how much real time passed. The
// player consumes exactly SampleRate samples per second: sending fewer starves
// it, and sending more overflows its buffer, which is heard as chopping once the
// buffer fills.
func (a *Audio) outputRate() string {
	if a.firstEmit.IsZero() || a.codec == nil {
		return "none"
	}
	clock := a.now
	if clock == nil {
		clock = time.Now
	}
	elapsed := clock().Sub(a.firstEmit).Seconds()
	if elapsed <= 0 {
		return "none"
	}
	want := float64(a.codec.SampleRate())
	got := float64(a.samplesOut) / elapsed
	return fmt.Sprintf("%.0f/s want %.0f/s (%.0f%%)", got, want, got/want*100)
}

// droppedLate totals the frames discarded for arriving past their moment.
func (a *Audio) droppedLate() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, s := range a.streams {
		n += s.dropped
	}
	return n
}
