package audio

import "wacalls/internal/voip/core"

// maxMixLag is how many frames one stream may run ahead before the mix stops
// waiting for a silent one. Three frames is 180 ms: long enough to absorb normal
// arrival skew, short enough that a participant who stopped delivering does not
// hold everyone else's audio.
const maxMixLag = 3

// inboundStream is one sender's receive pipeline: reorder by sequence, decode in
// order, conceal what was lost. Each participant needs its own because sequence
// numbers are per stream and the MLow decoder carries state between frames.
type inboundStream struct {
	jitter    *jitterBuffer
	codec     core.AudioCodec
	lastFrame []float32
	concealed int
	ready     [][]float32
}

func newInboundStream(codec core.AudioCodec) *inboundStream {
	return &inboundStream{jitter: newJitterBuffer(jitterDepth), codec: codec}
}

// push feeds one packet and queues whatever the jitter buffer released, decoded
// in sequence order so the stateful codec stays coherent.
func (s *inboundStream) push(seq uint16, payload []byte) {
	for _, fr := range s.jitter.push(seq, payload) {
		if fr.present {
			pcm, err := s.codec.Decode(fr.payload)
			if err != nil || len(pcm) == 0 {
				continue
			}
			s.lastFrame = pcm
			s.concealed = 0
			s.ready = append(s.ready, pcm)
			continue
		}
		if concealed := s.conceal(); concealed != nil {
			s.ready = append(s.ready, concealed)
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
