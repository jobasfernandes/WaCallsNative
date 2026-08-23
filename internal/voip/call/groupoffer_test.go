package call

import (
	"context"
	"log/slog"
	"testing"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/engine"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

// groupOfferNode monta o offer que o servidor manda ao convidar alguem para uma
// chamada de grupo ja ativa: carrega o roster e NAO carrega chave de chamada.
func groupOfferNode(callID string) *waBinary.Node {
	creator := types.NewJID("5511999990000", types.DefaultUserServer)
	info := offerGroupInfo()
	info.Attrs["call-id"] = callID
	return &waBinary.Node{
		Tag: "call",
		Attrs: waBinary.Attrs{
			"id": "STANZA1", "from": creator,
		},
		Content: []waBinary.Node{{
			Tag: "offer",
			Attrs: waBinary.Attrs{
				"call-id": callID, "call-creator": creator, "joinable": "1",
			},
			Content: []waBinary.Node{
				{Tag: "audio", Attrs: waBinary.Attrs{"enc": "opus", "rate": "16000"}},
				info,
			},
		}},
	}
}

func groupClient(sock *recordingSock) *Client {
	return NewClient(sock, slog.Default(), func() []engine.Extension { return nil }, 0,
		func(string, *CallManager) {}, func(string) core.CallObserver { return core.NopObserver{} })
}

// Um offer de grupo nao traz chave de chamada: ela chega depois por enc_rekey. Se
// a ausencia dela fizer a chamada ser rejeitada, todo o caminho de grupo fica
// inalcancavel.
func TestGroupOfferIsNotRejectedForMissingCallKey(t *testing.T) {
	sock := &recordingSock{}
	c := groupClient(sock)
	peer := types.NewJID("5511999990000", types.DefaultUserServer)

	c.HandleOffer(context.Background(), groupOfferNode("GCALL1"), peer)

	if c.Count() != 1 {
		t.Fatalf("registered %d calls, want the group call to be accepted", c.Count())
	}
	for _, n := range sock.sent {
		for _, child := range n.GetChildren() {
			if child.Tag == "reject" {
				t.Fatal("a group offer must not be rejected for lacking a call key")
			}
		}
	}
}

// O roster vem no proprio offer: aplica-lo ali evita esperar o primeiro
// group_update para saber quem esta na chamada.
func TestGroupOfferAppliesTheRosterFromTheOffer(t *testing.T) {
	sock := &recordingSock{}
	c := groupClient(sock)
	peer := types.NewJID("5511999990000", types.DefaultUserServer)

	c.HandleOffer(context.Background(), groupOfferNode("GCALL1"), peer)

	cm, ok := c.Get("GCALL1")
	if !ok {
		t.Fatal("the call must be registered")
	}
	state := cm.GroupState()
	if state == nil || state.Roster == nil {
		t.Fatal("the roster carried by the offer must be applied")
	}
	if len(state.Roster.Participants) != 2 {
		t.Errorf("participants = %d, want the two from the offer", len(state.Roster.Participants))
	}
}

// Uma oferta 1:1 sem chave continua sendo rejeitada: a mudanca vale so para grupo.
func TestDirectOfferWithoutCallKeyIsStillRejected(t *testing.T) {
	// Sem chave decifravel o offer 1:1 continua sendo recusado.
	sock := &recordingSock{err: context.DeadlineExceeded}
	c := groupClient(sock)
	peer := types.NewJID("5511999990000", types.DefaultUserServer)

	direct := &waBinary.Node{
		Tag:   "call",
		Attrs: waBinary.Attrs{"id": "STANZA1", "from": peer},
		Content: []waBinary.Node{{
			Tag:   "offer",
			Attrs: waBinary.Attrs{"call-id": "DCALL1", "call-creator": peer},
			Content: []waBinary.Node{
				{Tag: "audio", Attrs: waBinary.Attrs{"enc": "opus", "rate": "16000"}},
				// O <enc> e o que aciona a decifragem; sem ele ela nem e tentada.
				{Tag: "enc", Attrs: waBinary.Attrs{"type": "pkmsg", "v": "2"}, Content: []byte{0x01}},
			},
		}},
	}
	c.HandleOffer(context.Background(), direct, peer)

	if c.Count() != 0 {
		t.Errorf("a 1:1 offer with no decryptable key must still be rejected, got %d calls", c.Count())
	}
}

// A chamada de grupo entra marcada como tal, senao o caminho de grupo nunca roda.
func TestGroupOfferMarksTheCallAsGroup(t *testing.T) {
	sock := &recordingSock{}
	c := groupClient(sock)
	peer := types.NewJID("5511999990000", types.DefaultUserServer)

	c.HandleOffer(context.Background(), groupOfferNode("GCALL1"), peer)

	cm, _ := c.Get("GCALL1")
	if cm.GroupState() == nil {
		t.Fatal("the call must carry group state")
	}
}

// offerGroupInfo e o <group_info> que o servidor embute no offer de convite.
func offerGroupInfo() waBinary.Node {
	creator := types.NewJID("5511999990000", types.DefaultUserServer)
	return waBinary.Node{
		Tag: "group_info",
		Attrs: waBinary.Attrs{
			"call-id": "GCALL1", "call-creator": creator, "media": "audio",
			"transaction-id": "1", "connected-limit": "32",
		},
		Content: []waBinary.Node{userNode("111"), userNode("222")},
	}
}
