package call

import (
	"context"
	"log/slog"
	"testing"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/engine"
	"wacalls/internal/voip/media"
	"wacalls/internal/voip/transport"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

func testEpoch() []byte {
	epoch := make([]byte, 32)
	for i := range epoch {
		epoch[i] = byte(i + 1)
	}
	return epoch
}

// groupKeysCM monta um CallManager com SRTP pronto, como fica depois do
// handshake, para exercitar so o registro de chaves de participante.
func groupKeysCM(t *testing.T, sock *recordingSock) *CallManager {
	t.Helper()
	m := NewCallManager(sock, slog.Default())
	m.currentCall = &CallInfo{CallID: "CALL1", PeerJid: "peer:0@lid"}
	m.srtp = engine.NewSrtpManager(km(1), km(9), core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	return m
}

func participantSSRC(deviceJID string) uint32 {
	return media.GenerateSecureSsrc("CALL1", ensureDeviceJid(deviceJID), core.SsrcCounterAudio)
}

// senderFor monta um remetente que cifra como o participante cifraria: chave
// derivada da epoch com o device JID dele.
func senderFor(t *testing.T, epoch []byte, deviceJID string) *engine.SrtpManager {
	t.Helper()
	sendKM, err := media.DerivePerJidSrtpKey(epoch, ensureDeviceJid(deviceJID))
	if err != nil {
		t.Fatalf("DerivePerJidSrtpKey: %v", err)
	}
	return engine.NewSrtpManager(sendKM, km(99), core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
}

// O teste que importa: audio de dois participantes, cada um com sua chave,
// autentica de ponta a ponta pelo SrtpManager real.
func TestGroupKeysAuthenticateTwoParticipants(t *testing.T) {
	sock := &recordingSock{epoch: testEpoch()}
	m := groupKeysCM(t, sock)
	ctx := context.Background()

	m.HandleControl(ctx, controlNode(groupUpdateNode(7, userNode("111"), userNode("222"))))
	m.HandleControl(ctx, controlNode(encRekeyNode(7)))

	for _, user := range []string{"111", "222"} {
		t.Run("participant "+user, func(t *testing.T) {
			deviceJID := types.JID{User: user, Server: types.HiddenUserServer}.String()
			sender := senderFor(t, testEpoch(), deviceJID)
			pkt := &media.RtpPacket{
				Header: media.NewRtpHeader(core.PayloadTypeWhatsAppOpus, 1, 0,
					participantSSRC(deviceJID)),
				Payload: []byte{0xAA, 0xBB},
			}
			wire, err := sender.Protect(pkt)
			if err != nil {
				t.Fatalf("Protect: %v", err)
			}
			got, err := m.srtp.Unprotect(wire)
			if err != nil {
				t.Fatalf("a participant's audio must authenticate: %v", err)
			}
			if len(got.Payload) != 2 || got.Payload[0] != 0xAA {
				t.Errorf("payload = %x", got.Payload)
			}
		})
	}
}

// As duas ordens de chegada tem de levar ao mesmo estado: o roster pode vir
// antes da epoch ou depois.
func TestGroupKeysRegisterInEitherOrder(t *testing.T) {
	deviceJID := types.JID{User: "111", Server: types.HiddenUserServer}.String()
	for _, order := range []struct {
		name  string
		nodes []waBinary.Node
	}{
		{"roster first", []waBinary.Node{groupUpdateNode(7, userNode("111")), encRekeyNode(7)}},
		{"epoch first", []waBinary.Node{encRekeyNode(7), groupUpdateNode(7, userNode("111"))}},
	} {
		t.Run(order.name, func(t *testing.T) {
			sock := &recordingSock{epoch: testEpoch()}
			m := groupKeysCM(t, sock)
			for _, n := range order.nodes {
				m.HandleControl(context.Background(), controlNode(n))
			}
			sender := senderFor(t, testEpoch(), deviceJID)
			pkt := &media.RtpPacket{
				Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppOpus, 1, 0, participantSSRC(deviceJID)),
				Payload: []byte{0x01},
			}
			wire, _ := sender.Protect(pkt)
			if _, err := m.srtp.Unprotect(wire); err != nil {
				t.Fatalf("audio must authenticate regardless of arrival order: %v", err)
			}
		})
	}
}

// Sem a epoch nao ha o que derivar: nada pode ser registrado.
func TestGroupKeysNotRegisteredWithoutEpoch(t *testing.T) {
	sock := &recordingSock{}
	m := groupKeysCM(t, sock)
	m.HandleControl(context.Background(), controlNode(groupUpdateNode(7, userNode("111"))))

	deviceJID := types.JID{User: "111", Server: types.HiddenUserServer}.String()
	sender := senderFor(t, testEpoch(), deviceJID)
	pkt := &media.RtpPacket{
		Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppOpus, 1, 0, participantSSRC(deviceJID)),
		Payload: []byte{0x01},
	}
	wire, _ := sender.Protect(pkt)
	if _, err := m.srtp.Unprotect(wire); err == nil {
		t.Fatal("without the epoch no participant key can exist, so this must not authenticate")
	}
}

// Um participante que entra depois e registrado no roster seguinte.
func TestGroupKeysRegisterLateJoiner(t *testing.T) {
	sock := &recordingSock{epoch: testEpoch()}
	m := groupKeysCM(t, sock)
	ctx := context.Background()
	m.HandleControl(ctx, controlNode(groupUpdateNode(7, userNode("111"))))
	m.HandleControl(ctx, controlNode(encRekeyNode(7)))
	m.HandleControl(ctx, controlNode(groupUpdateNode(8, userNode("111"), userNode("333"))))

	deviceJID := types.JID{User: "333", Server: types.HiddenUserServer}.String()
	sender := senderFor(t, testEpoch(), deviceJID)
	pkt := &media.RtpPacket{
		Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppOpus, 1, 0, participantSSRC(deviceJID)),
		Payload: []byte{0x01},
	}
	wire, _ := sender.Protect(pkt)
	if _, err := m.srtp.Unprotect(wire); err != nil {
		t.Fatalf("a participant that joined later must authenticate: %v", err)
	}
}

// Registrar a nossa propria chave como recepcao faria o eco do relay autenticar
// como se fosse audio de outro participante.
func TestGroupKeysExcludeOwnDevice(t *testing.T) {
	own := types.JID{User: "999", Server: types.HiddenUserServer}
	sock := &recordingSock{epoch: testEpoch(), ownLID: own}
	m := groupKeysCM(t, sock)
	ctx := context.Background()
	m.HandleControl(ctx, controlNode(groupUpdateNode(7, userNode("111"), userNode("999"))))
	m.HandleControl(ctx, controlNode(encRekeyNode(7)))

	sender := senderFor(t, testEpoch(), own.String())
	pkt := &media.RtpPacket{
		Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppOpus, 1, 0, participantSSRC(own.String())),
		Payload: []byte{0x01},
	}
	wire, _ := sender.Protect(pkt)
	if _, err := m.srtp.Unprotect(wire); err == nil {
		t.Fatal("our own device must not be registered as a receive key")
	}
}

// Uma chamada 1:1 nunca registra chave de participante.
func TestGroupKeysUntouchedOnDirectCall(t *testing.T) {
	sock := &recordingSock{}
	m := groupKeysCM(t, sock)
	if m.GroupState() != nil {
		t.Fatal("a 1:1 call must not carry group state")
	}
	// O caminho 1:1 continua autenticando com a chave unica de sempre.
	sender := engine.NewSrtpManager(km(9), km(1), core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	pkt := &media.RtpPacket{
		Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppOpus, 1, 0, 4242),
		Payload: []byte{0x01},
	}
	wire, _ := sender.Protect(pkt)
	if _, err := m.srtp.Unprotect(wire); err != nil {
		t.Fatalf("the 1:1 receive path must keep working: %v", err)
	}
}

// O reenvio do allocate e disparado pela mudanca do conjunto de PIDs, nao por um
// transaction id de relay: um participante que entra sem o relay bumpar esse id
// ficaria com subscription velha e nunca teria a midia assinada.
func TestGroupAllocateResendsOnPIDChange(t *testing.T) {
	sock := &recordingSock{epoch: testEpoch(), ownLID: lidJID("999")}
	m := groupKeysCM(t, sock)
	var configs []transport.GroupAllocateConfig
	m.relay = &fakeRelay{onGroupAllocate: func(cfg transport.GroupAllocateConfig) bool {
		configs = append(configs, cfg)
		// Espelha a semantica real: muda quando o conjunto de PIDs muda.
		if len(configs) == 1 {
			return true
		}
		prev := configs[len(configs)-2].PIDs
		return len(prev) != len(cfg.PIDs)
	}}
	ctx := context.Background()

	m.HandleControl(ctx, controlNode(groupUpdateNode(7, userNode("111"))))
	m.HandleControl(ctx, controlNode(encRekeyNode(7)))
	if len(configs) == 0 {
		t.Fatal("the relay must be told the group shape once roster and epoch are in")
	}
	first := len(configs[0].PIDs)

	// Um participante a mais tem de produzir um conjunto de PIDs maior.
	m.HandleControl(ctx, controlNode(groupUpdateNode(8, userNode("111"), userNode("222"))))
	last := configs[len(configs)-1]
	if len(last.PIDs) <= first {
		t.Errorf("PIDs = %v, want more than the %d before the join", last.PIDs, first)
	}
	// Os nove SSRCs de stream nao podem colidir com o de app-data.
	for i, ssrc := range last.Streams {
		if ssrc == last.AppDataSSRC {
			t.Errorf("stream slot %d collides with the app-data SSRC", i)
		}
	}
}

// A chave de envio passa a vir da epoch: sem isso ninguem decodifica o nosso audio.
func TestGroupSendKeyComesFromTheEpoch(t *testing.T) {
	sock := &recordingSock{epoch: testEpoch(), ownLID: lidJID("999")}
	m := groupKeysCM(t, sock)
	m.relay = &fakeRelay{}
	ctx := context.Background()
	m.HandleControl(ctx, controlNode(groupUpdateNode(7, userNode("111"))))
	m.HandleControl(ctx, controlNode(encRekeyNode(7)))

	pkt := &media.RtpPacket{
		Header:  media.NewRtpHeader(core.PayloadTypeWhatsAppOpus, 1, 0, 1234),
		Payload: []byte{0x01, 0x02},
	}
	wire, err := m.srtp.Protect(pkt)
	if err != nil {
		t.Fatalf("Protect: %v", err)
	}
	ourDeviceJID := ensureDeviceJid(m.ownCredJid())
	expected, err := media.DerivePerJidSrtpKey(testEpoch(), ourDeviceJID)
	if err != nil {
		t.Fatalf("DerivePerJidSrtpKey: %v", err)
	}
	receiver := engine.NewSrtpManager(km(50), expected, core.SRTPRecvAuthTagLen, core.SRTPSendAuthTagLen)
	if _, err := receiver.Unprotect(wire); err != nil {
		t.Fatalf("a participant holding the epoch key must decode our audio: %v", err)
	}
}

func lidJID(user string) types.JID {
	return types.JID{User: user, Server: types.HiddenUserServer}
}
