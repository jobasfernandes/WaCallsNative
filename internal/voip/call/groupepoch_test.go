package call

import (
	"context"
	"strconv"
	"testing"

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
