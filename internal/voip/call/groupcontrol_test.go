package call

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"testing"

	"wacalls/internal/voip/core"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

// recordingSock captura o que sai e devolve uma epoch controlada.
type recordingSock struct {
	fakeSock
	mu    sync.Mutex
	sent  []waBinary.Node
	epoch []byte
	err   error
}

func (s *recordingSock) SendNode(_ context.Context, node waBinary.Node) error {
	s.mu.Lock()
	s.sent = append(s.sent, node)
	s.mu.Unlock()
	return nil
}

func (s *recordingSock) Query(_ context.Context, node waBinary.Node) (*waBinary.Node, error) {
	s.mu.Lock()
	s.sent = append(s.sent, node)
	s.mu.Unlock()
	return nil, nil
}

func (s *recordingSock) DecryptCallKey(_ context.Context, _ types.JID, _ *waBinary.Node) ([]byte, error) {
	return s.epoch, s.err
}

func (s *recordingSock) acks() []waBinary.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []waBinary.Node
	for _, n := range s.sent {
		if n.Tag == "ack" {
			out = append(out, n)
		}
	}
	return out
}

func groupCM(t *testing.T, sock *recordingSock) *CallManager {
	t.Helper()
	m := NewCallManager(sock, slog.Default())
	m.currentCall = &CallInfo{CallID: "CALL1", PeerJid: "peer:0@lid"}
	return m
}

// controlNode monta o <call> cru que o whatsmeow entrega como evento nao tipado.
func controlNode(action waBinary.Node) *waBinary.Node {
	return &waBinary.Node{
		Tag: "call",
		Attrs: waBinary.Attrs{
			"id": "STANZA1", "from": types.NewJID("5511999990000", types.DefaultUserServer),
		},
		Content: []waBinary.Node{action},
	}
}

func groupUpdateNode(transactionID uint32, participants ...waBinary.Node) waBinary.Node {
	creator := types.NewJID("5511999990000", types.DefaultUserServer)
	return waBinary.Node{
		Tag:   "group_update",
		Attrs: waBinary.Attrs{"call-id": "CALL1", "call-creator": creator},
		Content: []waBinary.Node{{
			Tag: "group_info",
			Attrs: waBinary.Attrs{
				"call-id": "CALL1", "call-creator": creator, "media": "audio",
				"transaction-id":  strconv.FormatUint(uint64(transactionID), 10),
				"connected-limit": "32",
			},
			Content: participants,
		}},
	}
}

func userNode(user string) waBinary.Node {
	j := types.JID{User: user, Server: types.HiddenUserServer}
	return waBinary.Node{
		Tag:     "user",
		Attrs:   waBinary.Attrs{"jid": j, "state": "connected"},
		Content: []waBinary.Node{{Tag: "device", Attrs: waBinary.Attrs{"jid": j, "pid": "1"}}},
	}
}

func encRekeyNode(transactionID uint32) waBinary.Node {
	return waBinary.Node{
		Tag: "enc_rekey",
		Attrs: waBinary.Attrs{
			"call-id": "CALL1", "call-creator": types.NewJID("5511999990000", types.DefaultUserServer),
			"transaction-id": strconv.FormatUint(uint64(transactionID), 10),
		},
		Content: []waBinary.Node{
			{Tag: "encopt", Attrs: waBinary.Attrs{"keygen": "2"}},
			{Tag: "enc", Attrs: waBinary.Attrs{"type": "pkmsg", "v": "2"}, Content: []byte{0x01, 0x02}},
		},
	}
}

// Uma chamada 1:1 nunca ganha estado de grupo.
func TestDirectCallHasNoGroupState(t *testing.T) {
	m := groupCM(t, &recordingSock{})
	if m.GroupState() != nil {
		t.Fatal("a 1:1 call must not carry group state")
	}
}

func TestHandleControlAppliesRoster(t *testing.T) {
	sock := &recordingSock{}
	m := groupCM(t, sock)
	m.HandleControl(context.Background(), controlNode(groupUpdateNode(7, userNode("111"), userNode("222"))))

	state := m.GroupState()
	if state == nil {
		t.Fatal("a group_update must install group state")
	}
	if state.TransactionID != 7 {
		t.Errorf("transactionID = %d, want 7", state.TransactionID)
	}
	if state.Roster == nil || len(state.Roster.Participants) != 2 {
		t.Fatalf("roster = %+v, want two participants", state.Roster)
	}
}

// Um snapshot mais velho reintroduziria participantes que ja sairam.
func TestHandleControlDiscardsOlderRoster(t *testing.T) {
	sock := &recordingSock{}
	m := groupCM(t, sock)
	ctx := context.Background()
	m.HandleControl(ctx, controlNode(groupUpdateNode(5, userNode("111"), userNode("222"))))
	m.HandleControl(ctx, controlNode(groupUpdateNode(3, userNode("111"))))

	state := m.GroupState()
	if state.TransactionID != 5 {
		t.Errorf("transactionID = %d, want the newer 5", state.TransactionID)
	}
	if len(state.Roster.Participants) != 2 {
		t.Errorf("participants = %d, want the two from the newer snapshot", len(state.Roster.Participants))
	}
}

// O servidor reenvia snapshots; reaplicar o mesmo e trabalho a toa.
func TestHandleControlDiscardsRepeatedRoster(t *testing.T) {
	sock := &recordingSock{}
	m := groupCM(t, sock)
	ctx := context.Background()
	m.HandleControl(ctx, controlNode(groupUpdateNode(5, userNode("111"), userNode("222"))))
	m.HandleControl(ctx, controlNode(groupUpdateNode(5, userNode("111"))))

	if n := len(m.GroupState().Roster.Participants); n != 2 {
		t.Errorf("participants = %d, want the original two", n)
	}
}

func TestHandleControlInstallsEpoch(t *testing.T) {
	epoch := make([]byte, 32)
	for i := range epoch {
		epoch[i] = byte(i)
	}
	sock := &recordingSock{epoch: epoch}
	m := groupCM(t, sock)
	m.HandleControl(context.Background(), controlNode(encRekeyNode(9)))

	state := m.GroupState()
	if state == nil || len(state.Epoch) != 32 {
		t.Fatalf("epoch = %v, want 32 bytes installed", state)
	}
	for i, b := range state.Epoch {
		if b != byte(i) {
			t.Fatalf("epoch byte %d = %d, want %d", i, b, i)
		}
	}
}

// Uma epoch que nao decripta nao pode derrubar a chamada: o media plane apenas
// nao sobe, de forma observavel.
func TestHandleControlSurvivesUndecryptableEpoch(t *testing.T) {
	sock := &recordingSock{err: context.DeadlineExceeded}
	m := groupCM(t, sock)
	m.HandleControl(context.Background(), controlNode(encRekeyNode(9)))

	if m.currentCall == nil || m.currentCall.IsEnded() {
		t.Fatal("a failed epoch must not end the call")
	}
	if state := m.GroupState(); state != nil && len(state.Epoch) != 0 {
		t.Errorf("epoch = %x, want none installed", state.Epoch)
	}
}

// Um roster malformado tambem nao derruba a chamada.
func TestHandleControlSurvivesMalformedRoster(t *testing.T) {
	sock := &recordingSock{}
	m := groupCM(t, sock)
	bad := waBinary.Node{
		Tag:   "group_update",
		Attrs: waBinary.Attrs{"call-id": "CALL1", "call-creator": types.NewJID("1", types.DefaultUserServer)},
	}
	m.HandleControl(context.Background(), controlNode(bad))

	if m.currentCall == nil || m.currentCall.IsEnded() {
		t.Fatal("a malformed roster must not end the call")
	}
	if m.GroupState() != nil {
		t.Error("a malformed roster must not install state")
	}
}

// Os cinco nos de controle exigem ack tipado. Responder e obrigatorio mesmo para
// os que ainda nao sao tratados: sem ack o peer reverte o que anunciou.
func TestHandleControlAcksEveryControlAction(t *testing.T) {
	creator := types.NewJID("5511999990000", types.DefaultUserServer)
	actions := []waBinary.Node{
		groupUpdateNode(1, userNode("111")),
		encRekeyNode(1),
		{Tag: "waiting_room_update", Attrs: waBinary.Attrs{"call-id": "CALL1", "call-creator": creator}},
		{Tag: "user_action", Attrs: waBinary.Attrs{"call-id": "CALL1", "call-creator": creator}},
		{Tag: "screen_share", Attrs: waBinary.Attrs{"call-id": "CALL1", "call-creator": creator}},
	}
	for _, action := range actions {
		t.Run(action.Tag, func(t *testing.T) {
			sock := &recordingSock{epoch: make([]byte, 32)}
			m := groupCM(t, sock)
			m.HandleControl(context.Background(), controlNode(action))

			acks := sock.acks()
			if len(acks) != 1 {
				t.Fatalf("acks = %d, want exactly 1", len(acks))
			}
			if got := acks[0].Attrs["type"]; got != action.Tag {
				t.Errorf("ack type = %v, want %q", got, action.Tag)
			}
			if acks[0].Attrs["class"] != "call" {
				t.Errorf("ack class = %v, want call", acks[0].Attrs["class"])
			}
		})
	}
}

// Um no de controle de outra chamada nao pode tocar esta.
func TestHandleControlIgnoresOtherCall(t *testing.T) {
	sock := &recordingSock{}
	m := groupCM(t, sock)
	other := groupUpdateNode(7, userNode("111"))
	other.Attrs["call-id"] = "OTHER"
	other.GetChildren()[0].Attrs["call-id"] = "OTHER"
	m.HandleControl(context.Background(), controlNode(other))

	if m.GroupState() != nil {
		t.Error("a control node for another call must not install state here")
	}
}

// O Client roteia por call-id e nao entra em panico com uma chamada desconhecida.
func TestClientHandleControlUnknownCall(t *testing.T) {
	c := NewClient(&recordingSock{}, slog.Default(), nil, 0,
		func(string, *CallManager) {}, func(string) core.CallObserver { return core.NopObserver{} })
	c.HandleControl(context.Background(), controlNode(groupUpdateNode(1, userNode("111"))))
}
