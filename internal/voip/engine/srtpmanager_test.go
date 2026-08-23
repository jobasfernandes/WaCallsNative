package engine

import (
	"bytes"
	"testing"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
)

func testKM(seed byte) core.SrtpKeyingMaterial {
	mk := make([]byte, 16)
	ms := make([]byte, 14)
	for i := range mk {
		mk[i] = seed + byte(i)
	}
	for i := range ms {
		ms[i] = seed*2 + byte(i)
	}
	return core.SrtpKeyingMaterial{MasterKey: mk, MasterSalt: ms}
}

func rtpPkt(ssrc uint32, seq uint16, payload []byte) *media.RtpPacket {
	return &media.RtpPacket{
		Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppOpus, seq, uint32(seq), ssrc),
		Payload: payload,
	}
}

func mustProtect(t *testing.T, m *SrtpManager, pkt *media.RtpPacket) []byte {
	t.Helper()
	wire, err := m.Protect(pkt)
	if err != nil {
		t.Fatalf("Protect: %v", err)
	}
	return wire
}

func TestSrtpManager_Roundtrip(t *testing.T) {
	k1, k2 := testKM(1), testKM(9)
	sender := NewSrtpManager(k1, k2, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	receiver := NewSrtpManager(k2, k1, core.SRTPRecvAuthTagLen, core.SRTPSendAuthTagLen)

	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	got, err := receiver.Unprotect(mustProtect(t, sender, rtpPkt(7, 1, payload)))
	if err != nil {
		t.Fatalf("Unprotect: %v", err)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Fatalf("roundtrip mismatch: got %x want %x", got.Payload, payload)
	}
}

func TestSrtpManager_RejectsDuplicatePacket(t *testing.T) {
	k1, k2 := testKM(1), testKM(9)
	sender := NewSrtpManager(k1, k2, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	receiver := NewSrtpManager(k2, k1, core.SRTPRecvAuthTagLen, core.SRTPSendAuthTagLen)

	wire := mustProtect(t, sender, rtpPkt(7, 42, []byte{0xAA, 0xBB}))
	if _, err := receiver.Unprotect(wire); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if _, err := receiver.Unprotect(wire); err == nil {
		t.Fatal("duplicate packet must not be decoded twice")
	}
}

func TestSrtpManager_PerSsrcRocIsolation(t *testing.T) {
	k1, k2 := testKM(1), testKM(9)
	sender := NewSrtpManager(k1, k2, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	receiver := NewSrtpManager(k2, k1, core.SRTPRecvAuthTagLen, core.SRTPSendAuthTagLen)

	const a, b = uint32(1111), uint32(2222)
	payA1 := []byte{0xA1, 0xA1, 0xA1, 0xA1}
	payB := []byte{0xB0, 0xB0, 0xB0, 0xB0}
	payA2 := []byte{0xA2, 0xA2, 0xA2, 0xA2}

	wireA1 := mustProtect(t, sender, rtpPkt(a, 100, payA1))
	wireB := mustProtect(t, sender, rtpPkt(b, 60000, payB))
	wireA2 := mustProtect(t, sender, rtpPkt(a, 101, payA2))

	gotA1, err := receiver.Unprotect(wireA1)
	if err != nil || !bytes.Equal(gotA1.Payload, payA1) {
		t.Fatalf("A1: err=%v payload=%x", err, gotA1.Payload)
	}
	gotB, err := receiver.Unprotect(wireB)
	if err != nil || !bytes.Equal(gotB.Payload, payB) {
		t.Fatalf("B: err=%v payload=%x", err, gotB.Payload)
	}
	gotA2, err := receiver.Unprotect(wireA2)
	if err != nil || !bytes.Equal(gotA2.Payload, payA2) {
		t.Fatalf("A2 (ROC leaked across SSRC): err=%v payload=%x", err, gotA2.Payload)
	}
}

func TestSrtpManager_RekeyRecv(t *testing.T) {
	k1, k2, k3 := testKM(1), testKM(9), testKM(21)
	senderOld := NewSrtpManager(k1, k2, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	receiver := NewSrtpManager(k2, k1, core.SRTPRecvAuthTagLen, core.SRTPSendAuthTagLen)

	const ssrc = uint32(4242)
	old := []byte{0x11, 0x22, 0x33, 0x44}
	got, err := receiver.Unprotect(mustProtect(t, senderOld, rtpPkt(ssrc, 5, old)))
	if err != nil || !bytes.Equal(got.Payload, old) {
		t.Fatalf("pre-rekey: err=%v payload=%x", err, got.Payload)
	}

	receiver.RekeyRecv(k3)
	senderNew := NewSrtpManager(k3, k2, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	fresh := []byte{0x55, 0x66, 0x77, 0x88}
	got2, err := receiver.Unprotect(mustProtect(t, senderNew, rtpPkt(ssrc, 6, fresh)))
	if err != nil || !bytes.Equal(got2.Payload, fresh) {
		t.Fatalf("post-rekey: err=%v payload=%x", err, got2.Payload)
	}
}

// countingObs conta as chamadas de contabilidade de memoria e os drops.
type countingObs struct {
	core.NopObserver
	added    int64
	released int64
	drops    int
}

func (o *countingObs) AddMem(n int64)      { o.added += n }
func (o *countingObs) ReleaseMem(n int64)  { o.released += n }
func (o *countingObs) SrtpRecvDrop(string) { o.drops++ }

// Numa chamada de grupo cada participante cifra com a chave derivada do seu
// proprio device JID, entao o receptor precisa de uma chave por remetente.
func TestSrtpManager_PerSsrcRecvKeys(t *testing.T) {
	alice, bob, self := testKM(1), testKM(2), testKM(9)
	senderA := NewSrtpManager(alice, self, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	senderB := NewSrtpManager(bob, self, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)

	receiver := NewSrtpManager(self, testKM(99), core.SRTPRecvAuthTagLen, core.SRTPSendAuthTagLen)
	receiver.SetRecvKeyForSSRC(10, alice)
	receiver.SetRecvKeyForSSRC(20, bob)

	for _, tc := range []struct {
		name    string
		sender  *SrtpManager
		ssrc    uint32
		payload []byte
	}{
		{"alice", senderA, 10, []byte{0x01, 0x02}},
		{"bob", senderB, 20, []byte{0x03, 0x04}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := receiver.Unprotect(mustProtect(t, tc.sender, rtpPkt(tc.ssrc, 1, tc.payload)))
			if err != nil {
				t.Fatalf("Unprotect: %v", err)
			}
			if !bytes.Equal(got.Payload, tc.payload) {
				t.Fatalf("payload = %x, want %x", got.Payload, tc.payload)
			}
		})
	}
}

// Um remetente sem chave registrada continua caindo na chave unica e sendo
// rejeitado: registrar chaves nao pode afrouxar a autenticacao.
func TestSrtpManager_UnregisteredSenderStillRejected(t *testing.T) {
	alice, stranger, self := testKM(1), testKM(3), testKM(9)
	receiver := NewSrtpManager(self, testKM(99), core.SRTPRecvAuthTagLen, core.SRTPSendAuthTagLen)
	obs := &countingObs{}
	receiver.SetObserver(obs)
	receiver.SetRecvKeyForSSRC(10, alice)

	senderX := NewSrtpManager(stranger, self, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	if _, err := receiver.Unprotect(mustProtect(t, senderX, rtpPkt(30, 1, []byte{0x05}))); err == nil {
		t.Fatal("a sender with no registered key must not authenticate")
	}
}

// A corrida real: o pacote de um participante pode chegar antes do roster que
// traz a chave dele. Sem invalidar o contexto criado com a chave errada, ele
// ficaria mudo para sempre.
func TestSrtpManager_RegisteringKeyLateInvalidatesContext(t *testing.T) {
	alice, self := testKM(1), testKM(9)
	sender := NewSrtpManager(alice, self, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	receiver := NewSrtpManager(self, testKM(99), core.SRTPRecvAuthTagLen, core.SRTPSendAuthTagLen)

	// Chega antes da chave: falha, e cria um contexto com a chave errada.
	if _, err := receiver.Unprotect(mustProtect(t, sender, rtpPkt(10, 1, []byte{0x01}))); err == nil {
		t.Fatal("a packet arriving before its key must fail")
	}

	receiver.SetRecvKeyForSSRC(10, alice)

	got, err := receiver.Unprotect(mustProtect(t, sender, rtpPkt(10, 2, []byte{0x02})))
	if err != nil {
		t.Fatalf("after registering the key the sender must authenticate: %v", err)
	}
	if !bytes.Equal(got.Payload, []byte{0x02}) {
		t.Fatalf("payload = %x", got.Payload)
	}
}

// Substituir o contexto de um SSRC nao pode vazar contabilidade de memoria.
func TestSrtpManager_LateKeyDoesNotLeakMemory(t *testing.T) {
	alice, self := testKM(1), testKM(9)
	sender := NewSrtpManager(alice, self, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	receiver := NewSrtpManager(self, testKM(99), core.SRTPRecvAuthTagLen, core.SRTPSendAuthTagLen)
	obs := &countingObs{}
	receiver.SetObserver(obs)

	_, _ = receiver.Unprotect(mustProtect(t, sender, rtpPkt(10, 1, []byte{0x01})))
	receiver.SetRecvKeyForSSRC(10, alice)
	_, _ = receiver.Unprotect(mustProtect(t, sender, rtpPkt(10, 2, []byte{0x02})))
	receiver.Close()

	if obs.added != obs.released {
		t.Fatalf("memory accounting leaked: added %d, released %d", obs.added, obs.released)
	}
}

// Um rekey substitui todo o material de recepcao: chaves por participante da
// epoch antiga nao podem sobreviver a ele.
func TestSrtpManager_RekeyClearsPerSsrcKeys(t *testing.T) {
	alice, next, self := testKM(1), testKM(5), testKM(9)
	receiver := NewSrtpManager(self, testKM(99), core.SRTPRecvAuthTagLen, core.SRTPSendAuthTagLen)
	receiver.SetRecvKeyForSSRC(10, alice)
	receiver.RekeyRecv(next)

	// Depois do rekey o SSRC 10 volta a usar a chave nova, nao a de alice.
	senderAlice := NewSrtpManager(alice, self, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	if _, err := receiver.Unprotect(mustProtect(t, senderAlice, rtpPkt(10, 1, []byte{0x01}))); err == nil {
		t.Fatal("a key from before the rekey must not survive it")
	}
	senderNext := NewSrtpManager(next, self, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	if _, err := receiver.Unprotect(mustProtect(t, senderNext, rtpPkt(10, 2, []byte{0x02}))); err != nil {
		t.Fatalf("the post-rekey key must authenticate: %v", err)
	}
}
