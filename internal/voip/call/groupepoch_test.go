package call

import (
	"context"
	"encoding/base64"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/engine"
	"wacalls/internal/voip/media"
	"wacalls/internal/voip/transport"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

// encryptingSock devolve um no <to> por device, como o whatsmeow faz ao cifrar
// uma chave para varios destinos.
type encryptingSock struct {
	recordingSock
	encrypted [][]byte
	failFor   string
}

func (s *encryptingSock) CreateParticipantNodes(
	_ context.Context, devices []types.JID, key []byte, _ waBinary.Attrs,
) ([]waBinary.Node, bool, error) {
	s.mu.Lock()
	s.encrypted = append(s.encrypted, append([]byte(nil), key...))
	s.mu.Unlock()
	var out []waBinary.Node
	for i, d := range devices {
		if d.String() == s.failFor {
			continue
		}
		out = append(out, waBinary.Node{
			Tag:   "to",
			Attrs: waBinary.Attrs{"jid": d},
			Content: []waBinary.Node{{
				Tag:     "enc",
				Attrs:   waBinary.Attrs{"type": "pkmsg", "v": "2"},
				Content: []byte{0xE0, byte(i)},
			}},
		})
	}
	return out, false, nil
}

// rosterWithRekey monta um group_update que pede rekey, com dois participantes
// conectados e com PID, mais o nosso device.
func rosterWithRekey(transactionID uint32) waBinary.Node {
	node := groupUpdateNode(transactionID,
		connectedDevice("111", 1),
		connectedDevice("222", 0),
		connectedDevice("999", 2),
	)
	info := node.GetChildren()[0]
	info.Attrs["rekey"] = "1"
	return node
}

func connectedDevice(user string, pid int) waBinary.Node {
	j := types.JID{User: user, Server: types.HiddenUserServer}
	return waBinary.Node{
		Tag:   "user",
		Attrs: waBinary.Attrs{"jid": j, "state": "connected"},
		Content: []waBinary.Node{{
			Tag: "device",
			Attrs: waBinary.Attrs{
				"jid": j, "pid": strconv.Itoa(pid),
			},
		}},
	}
}

// O roster pede rekey e ninguem manda a epoch para nos: quem entra e quem a
// distribui. Sem isso o servidor rebaixa este device e o desconecta.
func TestRekeyRequestGeneratesAndDistributesTheEpoch(t *testing.T) {
	sock := &encryptingSock{}
	sock.ownLID = lidJID("999")
	m := groupKeysCM(t, &sock.recordingSock)
	m.sock = sock
	m.relay = &fakeRelay{}

	m.HandleControl(context.Background(), controlNode(rosterWithRekey(25)))

	sock.mu.Lock()
	defer sock.mu.Unlock()
	if len(sock.encrypted) != 1 {
		t.Fatalf("encrypted %d epochs, want exactly 1", len(sock.encrypted))
	}
	if len(sock.encrypted[0]) != 32 {
		t.Errorf("epoch is %d bytes, want 32", len(sock.encrypted[0]))
	}

	// Um enc_rekey por destinatario remoto, e nenhum para nos.
	var rekeys []waBinary.Node
	for _, n := range sock.sent {
		for _, child := range n.GetChildren() {
			if child.Tag == "enc_rekey" {
				rekeys = append(rekeys, n)
			}
		}
	}
	if len(rekeys) != 2 {
		t.Fatalf("sent %d enc_rekey stanzas, want one per remote connected device", len(rekeys))
	}
	for _, n := range rekeys {
		to, _ := n.Attrs["to"].(types.JID)
		if to.User == "999" {
			t.Error("no epoch is sent to our own device")
		}
	}

	// E a mesma epoch tem de ficar instalada aqui, senao decodificamos com uma
	// chave diferente da que distribuimos.
	state := m.GroupState()
	if state == nil || len(state.Epoch) != 32 {
		t.Fatal("the epoch we distributed must also be installed locally")
	}
	for i := range state.Epoch {
		if state.Epoch[i] != sock.encrypted[0][i] {
			t.Fatal("the installed epoch differs from the distributed one")
		}
	}
}

// O roster e reenviado; distribuir duas epochs para o mesmo transaction-id faria
// os participantes divergirem de chave.
func TestRekeyIsDistributedOncePerTransaction(t *testing.T) {
	sock := &encryptingSock{}
	sock.ownLID = lidJID("999")
	m := groupKeysCM(t, &sock.recordingSock)
	m.sock = sock
	m.relay = &fakeRelay{}
	ctx := context.Background()

	m.HandleControl(ctx, controlNode(rosterWithRekey(25)))
	m.HandleControl(ctx, controlNode(rosterWithRekey(25)))

	sock.mu.Lock()
	defer sock.mu.Unlock()
	if len(sock.encrypted) != 1 {
		t.Fatalf("generated %d epochs for one transaction, want 1", len(sock.encrypted))
	}
}

// Sem pedido de rekey nada e distribuido.
func TestNoRekeyRequestDistributesNothing(t *testing.T) {
	sock := &encryptingSock{}
	sock.ownLID = lidJID("999")
	m := groupKeysCM(t, &sock.recordingSock)
	m.sock = sock
	m.relay = &fakeRelay{}

	m.HandleControl(context.Background(), controlNode(groupUpdateNode(25, userNode("111"))))

	sock.mu.Lock()
	defer sock.mu.Unlock()
	if len(sock.encrypted) != 0 {
		t.Errorf("generated %d epochs without a rekey request, want none", len(sock.encrypted))
	}
}

// Um participante que ainda nao esta conectado, ou que nao tem PID, nao recebe:
// o servidor so encaminha midia entre devices com participante id.
func TestRekeySkipsParticipantsWithoutPID(t *testing.T) {
	sock := &encryptingSock{}
	sock.ownLID = lidJID("999")
	m := groupKeysCM(t, &sock.recordingSock)
	m.sock = sock
	m.relay = &fakeRelay{}

	// Um device sem PID: o servidor ainda nao roteia midia para ele, entao uma
	// chave enviada agora seria desperdicada.
	noPID := waBinary.Node{
		Tag: "user",
		Attrs: waBinary.Attrs{
			"jid": types.JID{User: "222", Server: types.HiddenUserServer}, "state": "connected",
		},
		Content: []waBinary.Node{{
			Tag:   "device",
			Attrs: waBinary.Attrs{"jid": types.JID{User: "222", Server: types.HiddenUserServer}},
		}},
	}
	node := groupUpdateNode(25,
		connectedDevice("111", 1),
		noPID,
		connectedDevice("999", 2),
	)
	node.GetChildren()[0].Attrs["rekey"] = "1"
	m.HandleControl(context.Background(), controlNode(node))

	sock.mu.Lock()
	defer sock.mu.Unlock()
	count := 0
	for _, n := range sock.sent {
		for _, child := range n.GetChildren() {
			if child.Tag == "enc_rekey" {
				count++
			}
		}
	}
	if count != 1 {
		t.Errorf("sent %d enc_rekey stanzas, want only the one device with a PID", count)
	}
}

// Sem SRTP nada do caminho de grupo roda: as chaves por participante, a chave de
// envio e o allocate estao todos atras desse gate. Numa chamada de grupo ele nao
// pode vir do handshake 1:1, porque nao ha chave de chamada.
func TestGroupCallBuildsItsOwnSrtpFromTheEpoch(t *testing.T) {
	sock := &encryptingSock{}
	sock.ownLID = lidJID("999")
	m := NewCallManager(sock, slog.Default())
	m.currentCall = &CallInfo{CallID: "CALL1", PeerJid: "peer:0@lid"}
	m.relay = &fakeRelay{}
	if m.srtp != nil {
		t.Fatal("precondition: a fresh call has no SRTP yet")
	}

	m.HandleControl(context.Background(), controlNode(rosterWithRekey(25)))

	if m.srtp == nil {
		t.Fatal("a group call must build its SRTP from the epoch, since it has no call key")
	}
	// E o audio de um participante tem de autenticar por ele.
	deviceJID := types.JID{User: "111", Server: types.HiddenUserServer}.String()
	state := m.GroupState()
	sendKM, err := media.DerivePerJidSrtpKey(state.Epoch, ensureDeviceJid(deviceJID))
	if err != nil {
		t.Fatalf("DerivePerJidSrtpKey: %v", err)
	}
	sender := engine.NewSrtpManager(sendKM, km(99), core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	pkt := &media.RtpPacket{
		Header: media.NewRtpHeader(core.PayloadTypeWhatsAppOpus, 1, 0,
			media.GenerateSecureSsrc("CALL1", ensureDeviceJid(deviceJID), core.SsrcCounterAudio)),
		Payload: []byte{0x01},
	}
	wire, _ := sender.Protect(pkt)
	if _, err := m.srtp.Unprotect(wire); err != nil {
		t.Fatalf("participant audio must authenticate once the group SRTP exists: %v", err)
	}
}

// Entrar por convite numa chamada de grupo nao tem conexao anterior para
// reaproveitar: os endpoints do roster sao o unico caminho que a midia tera.
func TestGroupRosterRelayDialsWhenNothingIsConnected(t *testing.T) {
	sock := &encryptingSock{}
	sock.ownLID = lidJID("999")
	m := NewCallManager(sock, slog.Default())
	m.currentCall = &CallInfo{CallID: "CALL1", PeerJid: "peer:0@lid"}
	var mu sync.Mutex
	var configured [][]transport.RelayConfig
	m.relay = &fakeRelay{noConn: true, onConfigure: func(r []transport.RelayConfig) {
		mu.Lock()
		configured = append(configured, r)
		mu.Unlock()
	}}

	m.HandleControl(context.Background(), controlNode(rosterWithGroupRelay(25)))

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(configured)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(configured) == 0 {
		t.Fatal("with nothing connected the roster relay must be dialed")
	}
	// O ICE le o token como texto. Mandar os bytes crus deixa a conexao em
	// checking ate estourar o timeout, e foi o que tirou este device da chamada.
	ep := configured[len(configured)-1][0]
	if want := base64.StdEncoding.EncodeToString([]byte("tok0")); ep.Token != want {
		t.Errorf("ICE token = %q, want the base64 form %q", ep.Token, want)
	}
	if ep.Key != "relaykey" {
		t.Errorf("ICE key = %q, want the relay key verbatim", ep.Key)
	}
}

// Uma chamada que virou grupo no lugar ja tem a conexao que carrega a midia.
// Discar de novo a abandona, e as novas ficam em checking ate o timeout.
func TestGroupRosterRelayDoesNotRedialOverALiveConnection(t *testing.T) {
	sock := &encryptingSock{}
	sock.ownLID = lidJID("999")
	m := NewCallManager(sock, slog.Default())
	m.currentCall = &CallInfo{CallID: "CALL1", PeerJid: "peer:0@lid"}
	var mu sync.Mutex
	dialed := 0
	m.relay = &fakeRelay{onConfigure: func([]transport.RelayConfig) {
		mu.Lock()
		dialed++
		mu.Unlock()
	}}

	m.HandleControl(context.Background(), controlNode(rosterWithGroupRelay(25)))

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if dialed != 0 {
		t.Fatalf("redialed %d times over a live connection, want none", dialed)
	}
}

// O allocate de grupo tem de sair com o token que o roster trouxe para aquele
// relay, e nao com o token do fluxo 1:1. Com o token errado o servidor ignora o
// allocate e este device nunca passa a receber midia do grupo.
func TestGroupRelayTokenReachesTheAllocate(t *testing.T) {
	sock := &encryptingSock{}
	sock.ownLID = lidJID("999")
	m := NewCallManager(sock, slog.Default())
	m.currentCall = &CallInfo{CallID: "CALL1", PeerJid: "peer:0@lid"}
	relay := &fakeRelay{}
	m.relay = relay

	m.HandleControl(context.Background(), controlNode(rosterWithGroupRelay(25)))

	cfg := relay.groupConfig()
	if cfg == nil {
		t.Fatal("the roster must configure the group allocate")
	}
	// Bytes crus aqui: o allocate STUN carrega o token, nao o texto do ICE.
	if got := string(cfg.Tokens["sao1"]); got != "tok0" {
		t.Errorf("group token for sao1 = %q, want the token the roster carried", got)
	}
	if got := string(cfg.Key); got != "relaykey" {
		t.Errorf("group relay key = %q, want the key the roster carried", got)
	}
}

// rosterWithGroupRelay monta um roster que pede rekey e carrega o bloco <relay>
// do grupo, irmao do <group_info>.
func rosterWithGroupRelay(transactionID uint32) waBinary.Node {
	node := rosterWithRekey(transactionID)
	children := node.GetChildren()
	children = append(children, waBinary.Node{
		Tag: "relay",
		Attrs: waBinary.Attrs{
			"uuid": "R1", "participant_uuid": "P1", "self_pid": "2",
			"transaction-id": strconv.FormatUint(uint64(transactionID), 10),
		},
		Content: []waBinary.Node{
			{Tag: "key", Content: []byte("relaykey")},
			{Tag: "token", Attrs: waBinary.Attrs{"id": "0"}, Content: []byte("tok0")},
			{
				Tag: "te2",
				Attrs: waBinary.Attrs{
					"relay_name": "sao1", "relay_id": "5", "token_id": "0", "c2r_rtt": "23",
				},
				Content: []byte{192, 168, 1, 10, 0x0d, 0x98},
			},
		},
	})
	node.Content = children
	return node
}

// O nosso device JID decide cada SSRC e cada chave que derivamos. Numa chamada de
// grupo por convite o relay 1:1 nao nos lista, e o fallback devolve o JID base,
// que vira o device :0. O roster e quem sabe qual device somos de verdade: usar o
// device errado deriva SSRCs e chaves que ninguem do outro lado reconhece.
func TestOurDeviceJidComesFromTheRoster(t *testing.T) {
	sock := &encryptingSock{}
	sock.ownLID = lidJID("999")
	m := NewCallManager(sock, slog.Default())
	m.currentCall = &CallInfo{CallID: "CALL1", PeerJid: "peer:0@lid"}
	m.relay = &fakeRelay{}

	// O roster nos lista no device 27, e nao ha RelayData nenhum.
	self := types.JID{User: "999", Server: types.HiddenUserServer, Device: 27}
	node := groupUpdateNode(25,
		connectedDevice("111", 1),
		waBinary.Node{
			Tag:   "user",
			Attrs: waBinary.Attrs{"jid": lidJID("999"), "state": "connected"},
			Content: []waBinary.Node{{
				Tag:   "device",
				Attrs: waBinary.Attrs{"jid": self, "pid": "2"},
			}},
		},
	)
	node.GetChildren()[0].Attrs["rekey"] = "1"
	m.HandleControl(context.Background(), controlNode(node))

	m.mu.Lock()
	got := m.ourDeviceJidLocked()
	m.mu.Unlock()
	if want := self.String(); got != want {
		t.Errorf("our device JID = %q, want %q from the roster", got, want)
	}
}

// Sem SSRC de envio o registro no relay sai pela porta de tras: sendRegistration
// desiste antes de mandar o allocate, o servidor nunca ve este device publicando
// midia e o rebaixa para invited. E sem sessao RTP nada de audio sai. Nenhum dos
// dois e montado pelo caminho 1:1 numa chamada de grupo, que nao tem chave de
// chamada nem passa por HandleCallOffer.
func TestGroupCallBuildsItsSendingMediaSession(t *testing.T) {
	sock := &encryptingSock{}
	sock.ownLID = lidJID("999")
	m := NewCallManager(sock, slog.Default())
	m.currentCall = &CallInfo{CallID: "CALL1", PeerJid: "peer:0@lid"}
	relay := &fakeRelay{}
	m.relay = relay

	self := types.JID{User: "999", Server: types.HiddenUserServer, Device: 27}
	node := groupUpdateNode(25,
		connectedDevice("111", 1),
		waBinary.Node{
			Tag:   "user",
			Attrs: waBinary.Attrs{"jid": lidJID("999"), "state": "connected"},
			Content: []waBinary.Node{{
				Tag: "device", Attrs: waBinary.Attrs{"jid": self, "pid": "2"},
			}},
		},
	)
	node.GetChildren()[0].Attrs["rekey"] = "1"
	m.HandleControl(context.Background(), controlNode(node))

	want := media.GenerateSecureSsrc("CALL1", self.String(), core.SsrcCounterAudio)
	m.mu.Lock()
	got, session := m.selfSsrc, m.rtpSession
	m.mu.Unlock()
	if got != want {
		t.Errorf("self SSRC = %d, want %d derived from our own device", got, want)
	}
	if session == nil {
		t.Error("a group call must build the RTP session it sends audio on")
	}
	if relay.sentSsrc() != want {
		t.Errorf("relay SSRC = %d, want %d: without it no allocate is sent",
			relay.sentSsrc(), want)
	}
}

// Sair de uma chamada de grupo tem de tirar so este device. O terminate 1:1 vai
// endereçado ao peer, que numa chamada de grupo e quem nos convidou, e o
// servidor o tira da chamada junto conosco.
func TestLeavingAGroupCallDoesNotEndItForTheInviter(t *testing.T) {
	sock := &recordingSock{}
	sock.ownLID = lidJID("999")
	m := NewCallManager(sock, slog.Default())
	inviter := types.NewJID("5511999990000", types.DefaultUserServer)
	call := NewIncomingCall("GCALL1", inviter.String(), inviter.String(), "", core.CallMediaTypeAudio)
	m.currentCall = call
	m.group = &GroupState{TransactionID: 1}
	m.relay = &fakeRelay{}

	if err := m.EndCall(context.Background(), core.EndCallReasonUserEnded); err != nil {
		t.Fatalf("EndCall: %v", err)
	}

	// O terminate sai numa goroutine.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sock.mu.Lock()
		n := len(sock.sent)
		sock.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	sock.mu.Lock()
	defer sock.mu.Unlock()
	var found bool
	for _, n := range sock.sent {
		for _, child := range n.GetChildren() {
			if child.Tag != "terminate" {
				continue
			}
			found = true
			to, _ := n.Attrs["to"].(types.JID)
			if to.Server != "call" {
				t.Errorf("terminate addressed to %s, want the call service", to)
			}
			if to.User == inviter.User {
				t.Error("a group terminate must not be addressed to the inviter")
			}
		}
	}
	if !found {
		t.Fatal("leaving must still send a terminate")
	}
}

// Alocar em todos os relays abertos faz cada allocate substituir o anterior, e
// acabamos ouvindo num relay por onde ninguem publica. Numa chamada recebida o
// relay de midia e o FNA.
func TestGroupMediaRelayIsASingleEndpoint(t *testing.T) {
	endpoints := []core.RelayEndpoint{
		{IP: "1.1.1.1", RelayName: "plain", AuthTokenID: "0"},
		{IP: "2.2.2.2", RelayName: "fna", IsFNA: true},
	}
	if got := selectGroupMediaRelay(endpoints, true); got == nil || got.RelayName != "fna" {
		t.Errorf("inbound call picked %v, want the FNA endpoint", got)
	}
	// Sem FNA, o endpoint com auth token vem primeiro.
	plain := []core.RelayEndpoint{
		{IP: "1.1.1.1", RelayName: "bare"},
		{IP: "2.2.2.2", RelayName: "authed", AuthTokenID: "1"},
	}
	if got := selectGroupMediaRelay(plain, true); got == nil || got.RelayName != "authed" {
		t.Errorf("picked %v, want the endpoint carrying an auth token", got)
	}
	if selectGroupMediaRelay(nil, true) != nil {
		t.Error("no endpoints must yield no relay")
	}
}

// O relay entrega a midia dos participantes embrulhada num header de
// forwarding. Sem desembrulhar, o pacote nao parece RTP nem STUN e e descartado
// sem deixar rastro: e por isso que nenhum participante e ouvido.
func TestForwardedMediaIsUnwrappedAndDelivered(t *testing.T) {
	sock := &encryptingSock{}
	sock.ownLID = lidJID("999")
	m := NewCallManager(sock, slog.Default())
	m.currentCall = &CallInfo{CallID: "CALL1", PeerJid: "peer:0@lid"}
	m.relay = &fakeRelay{}

	self := types.JID{User: "999", Server: types.HiddenUserServer, Device: 27}
	node := groupUpdateNode(25,
		connectedDevice("111", 1),
		waBinary.Node{
			Tag:   "user",
			Attrs: waBinary.Attrs{"jid": lidJID("999"), "state": "connected"},
			Content: []waBinary.Node{{
				Tag: "device", Attrs: waBinary.Attrs{"jid": self, "pid": "2"},
			}},
		},
	)
	node.GetChildren()[0].Attrs["rekey"] = "1"
	m.HandleControl(context.Background(), controlNode(node))

	device := ensureDeviceJid(types.JID{User: "111", Server: types.HiddenUserServer}.String())
	state := m.GroupState()
	sendKM, err := media.DerivePerJidSrtpKey(state.Epoch, device)
	if err != nil {
		t.Fatalf("DerivePerJidSrtpKey: %v", err)
	}
	sender := engine.NewSrtpManager(sendKM, km(99), core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	pkt := &media.RtpPacket{
		Header: media.NewRtpHeader(core.PayloadTypeWhatsAppOpus, 7, 160,
			media.GenerateSecureSsrc("CALL1", device, core.SsrcCounterAudio)),
		Payload: []byte{0xAA, 0xBB},
	}
	wire, err := sender.Protect(pkt)
	if err != nil {
		t.Fatalf("Protect: %v", err)
	}

	var got int
	m.registerRTPHandler(core.PayloadTypeWhatsAppOpus, func(*media.RtpPacket) { got++ })

	// Header de forwarding de 8 bytes, como o relay prefixa em modo
	// multi-participante.
	forwarded := append([]byte{0x09, 0x02, 0, 0, 0, 0, 0, 0}, wire...)
	m.onRelayData(forwarded)

	if got != 1 {
		t.Errorf("delivered %d forwarded packets, want 1", got)
	}
}

// O WhatsApp manda o mesmo audio sob 120 ou 121, e numa chamada de grupo a
// captura mostra 121 em todos os pacotes. Um receptor preso ao 120 nao ouve
// ninguem.
func TestAudioArrivesUnderEitherPayloadType(t *testing.T) {
	for _, pt := range []uint8{core.PayloadTypeWhatsAppOpus, core.PayloadTypeWhatsAppOpusAlt} {
		if !core.IsWhatsAppAudioPayload(pt) {
			t.Errorf("payload type %d must count as WhatsApp audio", pt)
		}
	}
	if core.IsWhatsAppAudioPayload(core.PayloadTypeWhatsAppAppData) {
		t.Error("app-data is not audio")
	}
}
