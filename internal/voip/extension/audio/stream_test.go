package audio

import "testing"

// O audio entregue ao player precisa de uma linha do tempo continua. Um salto no
// timestamp RTP significa silencio do outro lado, e sem preencher esse silencio
// o player recebe frames colados fora de hora.
func TestStreamPadsTimestampGapsWithSilence(t *testing.T) {
	s := newInboundStream(constantCodec{samples: 960})

	// Tres pacotes seguidos, com o terceiro 960 amostras adiante do esperado.
	s.push(1, 0, []byte{1})
	s.push(2, 960, []byte{1})
	s.push(3, 960*3, []byte{1})
	s.push(4, 960*4, []byte{1})
	s.push(5, 960*5, []byte{1})
	s.push(6, 960*6, []byte{1})

	var total int
	for _, frame := range s.ready {
		total += len(frame.pcm)
	}
	// Sem o preenchimento o total seria apenas a soma dos frames decodificados.
	if total <= len(s.ready)*960 {
		t.Errorf("total de amostras = %d em %d frames: o gap nao foi preenchido",
			total, len(s.ready))
	}
}

// Um salto grande demais nao vira minutos de silencio: a linha do tempo
// recomeca, como quando o outro lado reconecta.
func TestStreamRestartsTimelineOnHugeGap(t *testing.T) {
	s := newInboundStream(constantCodec{samples: 960})
	s.push(1, 0, []byte{1})
	s.push(2, 960, []byte{1})
	s.push(3, 960+900000, []byte{1})
	s.push(4, 960+900960, []byte{1})
	s.push(5, 960+901920, []byte{1})
	s.push(6, 960+902880, []byte{1})

	for _, frame := range s.ready {
		if len(frame.pcm) > 8000+960 {
			t.Fatalf("frame de %d amostras: o salto virou silencio sem limite", len(frame.pcm))
		}
	}
}

type constantCodec struct{ samples int }

func (c constantCodec) Decode([]byte) ([]float32, error) {
	return make([]float32, c.samples), nil
}
func (c constantCodec) Encode([]float32) ([]byte, error) { return nil, nil }
func (c constantCodec) FrameSize() int                   { return c.samples }
func (c constantCodec) SampleRate() int                  { return 16000 }
func (c constantCodec) Close()                           {}
