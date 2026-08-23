package engine

import (
	"errors"
	"sync"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
)

const srtpContextBytes = 4 * 1024

type SrtpManager struct {
	mu       sync.Mutex
	sendKM   core.SrtpKeyingMaterial
	recvKM   core.SrtpKeyingMaterial
	recvKeys map[uint32]core.SrtpKeyingMaterial
	sendAuth int
	recvAuth int
	send     map[uint32]*media.SrtpContext
	recv     map[uint32]*media.SrtpContext
	observer core.CallObserver
	mem      int64
}

func NewSrtpManager(sendKM, recvKM core.SrtpKeyingMaterial, sendAuth, recvAuth int) *SrtpManager {
	return &SrtpManager{
		sendKM:   sendKM,
		recvKM:   recvKM,
		recvKeys: map[uint32]core.SrtpKeyingMaterial{},
		sendAuth: sendAuth,
		recvAuth: recvAuth,
		send:     map[uint32]*media.SrtpContext{},
		recv:     map[uint32]*media.SrtpContext{},
		observer: core.NopObserver{},
	}
}

// SetRecvKeyForSSRC registers the receive key of one sender. A call with more
// than two parties has one key per participant device, so the single recvKM only
// answers for the 1:1 case; anything not registered here keeps using it.
//
// Any context already built for that SSRC is dropped: a packet can arrive before
// the roster that carries its key, and the context built with the wrong key would
// otherwise keep failing forever, leaving that participant silent.
func (m *SrtpManager) SetRecvKeyForSSRC(ssrc uint32, km core.SrtpKeyingMaterial) {
	m.mu.Lock()
	m.recvKeys[ssrc] = km
	var released int64
	if _, ok := m.recv[ssrc]; ok {
		delete(m.recv, ssrc)
		released = srtpContextBytes
		m.mem -= released
	}
	obs := m.observer
	m.mu.Unlock()
	if released > 0 {
		obs.ReleaseMem(released)
	}
}

func (m *SrtpManager) SetObserver(o core.CallObserver) {
	if o == nil {
		o = core.NopObserver{}
	}
	m.mu.Lock()
	m.observer = o
	m.mu.Unlock()
}

func (m *SrtpManager) Protect(pkt *media.RtpPacket) ([]byte, error) {
	if pkt == nil || pkt.Header == nil {
		return nil, errors.New("srtp: nil packet")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, ok := m.send[pkt.Header.Ssrc]
	if !ok {
		c, err := media.NewSrtpContext(m.sendKM, m.sendAuth)
		if err != nil {
			return nil, err
		}
		m.send[pkt.Header.Ssrc] = c
		m.mem += srtpContextBytes
		m.observer.AddMem(srtpContextBytes)
		ctx = c
	}
	return ctx.Protect(pkt)
}

func (m *SrtpManager) Unprotect(data []byte) (*media.RtpPacket, error) {
	if len(data) < 12 {
		return nil, errors.New("srtp: packet too short")
	}
	ssrc := media.RTPSsrc(data)
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, ok := m.recv[ssrc]
	if !ok {
		km, registered := m.recvKeys[ssrc]
		if !registered {
			km = m.recvKM
		}
		c, err := media.NewSrtpContext(km, m.recvAuth)
		if err != nil {
			return nil, err
		}
		m.recv[ssrc] = c
		m.mem += srtpContextBytes
		m.observer.AddMem(srtpContextBytes)
		ctx = c
	}
	return ctx.Unprotect(data)
}

// SetSendKey replaces the key outbound media is encrypted with. A group call
// keys off the shared epoch instead of the 1:1 call key, so contexts already
// built with the old key are dropped: keeping them would keep encrypting with a
// key the other participants cannot read.
func (m *SrtpManager) SetSendKey(sendKM core.SrtpKeyingMaterial) {
	m.mu.Lock()
	released := int64(len(m.send)) * srtpContextBytes
	m.mem -= released
	obs := m.observer
	m.sendKM = sendKM
	m.send = map[uint32]*media.SrtpContext{}
	m.mu.Unlock()
	if released > 0 {
		obs.ReleaseMem(released)
	}
}

func (m *SrtpManager) RekeyRecv(recvKM core.SrtpKeyingMaterial) {
	m.mu.Lock()
	released := int64(len(m.recv)) * srtpContextBytes
	m.mem -= released
	obs := m.observer
	m.recvKM = recvKM
	m.recv = map[uint32]*media.SrtpContext{}
	// A rekey replaces the whole receive side: per-participant keys derived from
	// the previous epoch cannot outlive it.
	m.recvKeys = map[uint32]core.SrtpKeyingMaterial{}
	m.mu.Unlock()
	if released > 0 {
		obs.ReleaseMem(released)
	}
}

func (m *SrtpManager) Close() {
	m.mu.Lock()
	released := m.mem
	obs := m.observer
	m.mem = 0
	m.send = map[uint32]*media.SrtpContext{}
	m.recv = map[uint32]*media.SrtpContext{}
	m.mu.Unlock()
	if released > 0 {
		obs.ReleaseMem(released)
	}
}
