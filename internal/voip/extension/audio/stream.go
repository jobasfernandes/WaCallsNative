package audio

import (
	"fmt"
	"sort"
	"strings"

	"wacalls/internal/voip/codec/mlow"
	"wacalls/internal/voip/core"
)

// maxMixLag is how many frames one stream may run ahead before the mix stops
// waiting for a silent one. Three frames is 180 ms: long enough to absorb normal
// arrival skew, short enough that a participant who stopped delivering does not
// hold everyone else's audio.
const maxMixLag = 3

// readyFrame is a decoded frame and the instant it belongs to. Mixing needs the
// instant: summing "the next frame of each stream" pairs frames that represent
// different moments, and the error grows with every round.
type readyFrame struct {
	pcm []float32
	at  uint32
}

// inboundStream is one sender's receive pipeline: reorder by sequence, decode in
// order, conceal what was lost. Each participant needs its own because sequence
// numbers are per stream and the MLow decoder carries state between frames.
type inboundStream struct {
	jitter    *jitterBuffer
	codec     core.AudioCodec
	lastFrame []float32
	concealed int
	ready     []readyFrame

	decodeFailures int
	dropped        int
	lastDecodeErr  error

	timelineSet   bool
	baseTimestamp uint32
	playedSamples uint64

	// tocHistogram counts the leading payload byte. A group call silences most
	// of its frames as inactive while a 1:1 call silences none, and the TOC is
	// where that verdict comes from: if the byte read here is not a TOC at all,
	// the payload carries something ahead of it.
	tocHistogram map[byte]int

	samplePayload    []byte
	samplePayloadLen int
	leadingZeros     int
	zeroFrames       int
}

func newInboundStream(codec core.AudioCodec) *inboundStream {
	return &inboundStream{jitter: newJitterBuffer(jitterDepth), codec: codec}
}

// push feeds one packet and queues whatever the jitter buffer released, decoded
// in sequence order so the stateful codec stays coherent.
// maxReadyFrames caps how much decoded audio one stream may hold. Holding the
// surplus instead of dropping it avoids gaps, but only up to a point: past this
// the queue never drains, latency grows without bound and the oldest frames are
// long past the moment they should have played. Ten frames is 600 ms, which
// covers the worst arrival gap measured on a live call.
const maxReadyFrames = 10

func (s *inboundStream) push(seq uint16, timestamp uint32, payload []byte) {
	for _, fr := range s.jitter.push(seq, payload) {
		if fr.present {
			s.noteTOC(fr.payload)
			pcm, err := s.codec.Decode(fr.payload)
			if err != nil || len(pcm) == 0 {
				// Counted, not ignored: a decoder that rejects every frame looks
				// exactly like media that never arrived.
				s.decodeFailures++
				if err != nil {
					s.lastDecodeErr = err
				}
				continue
			}
			s.lastFrame = pcm
			s.concealed = 0
			s.ready = append(s.ready, readyFrame{pcm: s.align(timestamp, pcm), at: timestamp})
			s.trimReady()
			continue
		}
		s.trimReady()
		if concealed := s.conceal(); concealed != nil {
			// O ocultamento ocupa o instante seguinte ao ultimo entregue.
			s.ready = append(s.ready, readyFrame{pcm: concealed, at: s.nextTimestamp()})
		}
	}
}

// conceal produces a frame to cover a lost packet: the last decoded frame faded
// out once, then silence for any consecutive losses.
func (s *inboundStream) conceal() []float32 {
	n := s.codec.FrameSize()
	if len(s.lastFrame) > 0 {
		n = len(s.lastFrame)
	}
	pcm := make([]float32, n)
	if s.concealed == 0 && len(s.lastFrame) > 1 {
		last := len(pcm) - 1
		for i := range pcm {
			pcm[i] = s.lastFrame[i] * (1 - float32(i)/float32(last))
		}
	}
	s.concealed++
	return pcm
}

func (s *inboundStream) close() {
	if s.codec != nil {
		s.codec.Close()
	}
}

// maxGapSamples bounds how much silence one timestamp jump may insert. Half a
// second of padding still plays as a pause; more than that is a sender that
// stopped and came back, and the timeline restarts instead.
const maxGapSamples = 8000

// align keeps the decoded audio on a continuous timeline. A jump in the RTP
// timestamp means the other side went quiet, and the gap has to be played as
// silence: handing the player frames back to back instead makes them run ahead
// of their own clock.
func (s *inboundStream) align(timestamp uint32, pcm []float32) []float32 {
	length := uint64(len(pcm))
	if !s.timelineSet {
		s.timelineSet = true
		s.baseTimestamp = timestamp
		s.playedSamples = length
		return pcm
	}
	target := uint64(timestamp - s.baseTimestamp)
	gap := int64(target) - int64(s.playedSamples)
	if gap < 0 || gap > maxGapSamples {
		s.baseTimestamp = timestamp
		s.playedSamples = length
		return pcm
	}
	if gap > 0 {
		padded := make([]float32, int(gap)+len(pcm))
		copy(padded[int(gap):], pcm)
		pcm = padded
	}
	s.playedSamples = target + length
	return pcm
}

func (s *inboundStream) noteTOC(payload []byte) {
	if len(payload) == 0 {
		return
	}
	if s.tocHistogram == nil {
		s.tocHistogram = map[byte]int{}
	}
	s.tocHistogram[payload[0]]++
	// The first byte alone cannot tell a zeroed TOC from a payload that starts
	// somewhere else: the head of the frame shows whether there is a prefix.
	if s.samplePayload == nil {
		n := min(len(payload), 12)
		s.samplePayload = append([]byte(nil), payload[:n]...)
		s.samplePayloadLen = len(payload)
	}
	if payload[0] == 0 {
		s.leadingZeros += countLeadingZeros(payload)
		s.zeroFrames++
	}
}

// countLeadingZeros reports how far into the frame the first non-zero byte sits.
func countLeadingZeros(payload []byte) int {
	for i, b := range payload {
		if b != 0 {
			return i
		}
	}
	return len(payload)
}

// topTOCBytes renders the most frequent leading bytes, most common first.
func (s *inboundStream) topTOCBytes(limit int) string {
	type pair struct {
		b byte
		n int
	}
	var pairs []pair
	for b, n := range s.tocHistogram {
		pairs = append(pairs, pair{b, n})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].n > pairs[j].n })
	var out []string
	for i, p := range pairs {
		if i == limit {
			break
		}
		toc := mlow.ParseSmplTOC(p.b)
		out = append(out, fmt.Sprintf("%#02x:%d(active=%t,ms=%d)", p.b, p.n, toc.Active, toc.FrameMs))
	}
	return strings.Join(out, " ")
}

// payloadShape renders the head of the first frame and how many bytes of zeros
// the frames start with on average. A constant non-zero offset means the audio
// begins past a prefix this decoder is feeding to the codec as if it were a TOC.
func (s *inboundStream) payloadShape() string {
	avg := 0
	if s.zeroFrames > 0 {
		avg = s.leadingZeros / s.zeroFrames
	}
	return fmt.Sprintf("head=%x len=%d avg_leading_zeros=%d/%d",
		s.samplePayload, s.samplePayloadLen, avg, s.zeroFrames)
}

// nextTimestamp is where this stream is on its own timeline.
func (s *inboundStream) nextTimestamp() uint32 {
	return s.baseTimestamp + uint32(s.playedSamples)
}

// trimReady drops the oldest frames once the queue is past what can still be
// played on time.
func (s *inboundStream) trimReady() {
	if len(s.ready) <= maxReadyFrames {
		return
	}
	drop := len(s.ready) - maxReadyFrames
	s.ready = append(s.ready[:0], s.ready[drop:]...)
	s.dropped += drop
}
