package signaling

import (
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

// Os tres verbos de link vao para o servico "call", nao para um device, e usam o
// requestID dado: a resposta e correlacionada por ele.
func TestCallLinkVerbsAddressTheCallService(t *testing.T) {
	cases := []struct {
		name  string
		build func() (waBinary.Node, error)
		tag   string
	}{
		{"create", func() (waBinary.Node, error) {
			return BuildCallLinkCreate(CallLinkMediaAudio, "REQ1")
		}, "link_create"},
		{"query", func() (waBinary.Node, error) {
			return BuildCallLinkQuery("TOKEN", CallLinkMediaAudio, "REQ1")
		}, "link_query"},
		{"join", func() (waBinary.Node, error) {
			return BuildCallLinkJoin("TOKEN", CallLinkMediaAudio, "REQ1")
		}, "link_join"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node, err := tc.build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if node.Attrs["id"] != "REQ1" {
				t.Errorf("id = %v, want REQ1", node.Attrs["id"])
			}
			to, _ := node.Attrs["to"].(types.JID)
			if to.Server != "call" || to.User != "" {
				t.Errorf("to = %v, want the bare call service", to)
			}
			if tag := node.GetChildren()[0].Tag; tag != tc.tag {
				t.Errorf("action = %q, want %q", tag, tc.tag)
			}
		})
	}
}

func TestCallLinkVerbsRejectInvalidInput(t *testing.T) {
	if _, err := BuildCallLinkCreate("photo", "REQ1"); err == nil {
		t.Error("an invalid media must be rejected")
	}
	if _, err := BuildCallLinkCreate(CallLinkMediaAudio, ""); err == nil {
		t.Error("an empty request ID must be rejected: the response is correlated by it")
	}
	if _, err := BuildCallLinkQuery("   ", CallLinkMediaAudio, "REQ1"); err == nil {
		t.Error("a blank token must be rejected")
	}
	if _, err := BuildCallLinkJoin("", CallLinkMediaAudio, "REQ1"); err == nil {
		t.Error("an empty token must be rejected")
	}
}

// Um link de video declara o decoder no join; um de audio nao.
func TestBuildCallLinkJoinCarriesVideoOnlyForVideoLinks(t *testing.T) {
	audio, err := BuildCallLinkJoin("TOKEN", CallLinkMediaAudio, "REQ1")
	if err != nil {
		t.Fatalf("audio join: %v", err)
	}
	for _, child := range audio.GetChildren()[0].GetChildren() {
		if child.Tag == "video" {
			t.Error("an audio link join must not declare video")
		}
	}
	video, err := BuildCallLinkJoin("TOKEN", CallLinkMediaVideo, "REQ1")
	if err != nil {
		t.Fatalf("video join: %v", err)
	}
	var found bool
	for _, child := range video.GetChildren()[0].GetChildren() {
		if child.Tag == "video" {
			found = true
			if child.Attrs["dec"] != "H264" {
				t.Errorf("video dec = %v, want H264", child.Attrs["dec"])
			}
		}
	}
	if !found {
		t.Error("a video link join must declare the video decoder")
	}
}

func linkAck(typ string, children ...waBinary.Node) *waBinary.Node {
	return &waBinary.Node{
		Tag:     "ack",
		Attrs:   waBinary.Attrs{"class": "call", "type": typ, "id": "REQ1", "from": jid("1")},
		Content: children,
	}
}

func TestParseCallLinkCreateAck(t *testing.T) {
	node := linkAck("link_create", waBinary.Node{
		Tag:   "link_create",
		Attrs: waBinary.Attrs{"token": "ABC123", "media": "audio"},
	})
	got, err := ParseCallLinkCreateAck(node)
	if err != nil {
		t.Fatalf("ParseCallLinkCreateAck: %v", err)
	}
	if got.Token != "ABC123" || got.Media != CallLinkMediaAudio {
		t.Errorf("link = %+v", got)
	}
}

func TestParseCallLinkCreateAckRejectsWrongEnvelope(t *testing.T) {
	cases := []struct {
		name string
		node *waBinary.Node
	}{
		{"nil", nil},
		{"not an ack", &waBinary.Node{Tag: "call"}},
		{"wrong type", linkAck("link_query")},
		{"missing payload", linkAck("link_create")},
		{"missing token", linkAck("link_create", waBinary.Node{
			Tag: "link_create", Attrs: waBinary.Attrs{"media": "audio"},
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseCallLinkCreateAck(tc.node); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestParseCallLinkQueryAck(t *testing.T) {
	node := linkAck("link_query", waBinary.Node{
		Tag: "link_query",
		Attrs: waBinary.Attrs{
			"token": "ABC123", "media": "audio", "link_creator": jid("5511999990000"),
		},
		Content: []waBinary.Node{
			{Tag: "waiting_room", Attrs: waBinary.Attrs{"enabled": "1", "is_admin": "1"}},
		},
	})
	got, err := ParseCallLinkQueryAck(node)
	if err != nil {
		t.Fatalf("ParseCallLinkQueryAck: %v", err)
	}
	if !got.WaitingRoomEnabled || !got.IsAdmin {
		t.Errorf("preview = %+v, want waiting room enabled and admin", got)
	}
	if got.Token != "ABC123" {
		t.Errorf("token = %q", got.Token)
	}
}

func waitingRoomNode(users ...waBinary.Node) waBinary.Node {
	return waBinary.Node{
		Tag: "waiting_room",
		Attrs: waBinary.Attrs{
			"call-id": "CALL1", "call-creator": jid("5511999990000"),
			"link-token": "ABC123", "media": "audio",
			"enabled": "1", "is_admin": "1", "transaction-id": "4",
		},
		Content: users,
	}
}

// Estar na sala de espera e derivado por AUSENCIA do group_info: o servidor
// responde um join retido com waiting_room e sem roster.
func TestParseCallLinkJoinAckHeldInWaitingRoom(t *testing.T) {
	node := linkAck("link_join", waitingRoomNode())
	got, err := ParseCallLinkJoinAck(node)
	if err != nil {
		t.Fatalf("ParseCallLinkJoinAck: %v", err)
	}
	if !got.InWaitingRoom {
		t.Error("a join answered with waiting_room and no group_info must be held")
	}
	if got.Group != nil {
		t.Error("no group snapshot is expected while held")
	}
	if got.CallID != "CALL1" || got.Token != "ABC123" {
		t.Errorf("join = %+v", got)
	}
}

// Com group_info o participante entrou: nao esta na sala, mesmo que a sala exista.
func TestParseCallLinkJoinAckAdmitted(t *testing.T) {
	node := linkAck("link_join", groupInfoNode())
	got, err := ParseCallLinkJoinAck(node)
	if err != nil {
		t.Fatalf("ParseCallLinkJoinAck: %v", err)
	}
	if got.InWaitingRoom {
		t.Error("a join answered with group_info must not be held")
	}
	if got.Group == nil {
		t.Fatal("the group snapshot must be parsed")
	}
	if got.CallID != "CALL1" {
		t.Errorf("callID = %q", got.CallID)
	}
}

// Os dois presentes significa admitido: o roster manda.
func TestParseCallLinkJoinAckWithBothIsAdmitted(t *testing.T) {
	node := linkAck("link_join", waitingRoomNode(), groupInfoNode())
	got, err := ParseCallLinkJoinAck(node)
	if err != nil {
		t.Fatalf("ParseCallLinkJoinAck: %v", err)
	}
	if got.InWaitingRoom {
		t.Error("with a group snapshot present the join is admitted")
	}
}

func TestParseCallLinkJoinAckRejectsEmpty(t *testing.T) {
	if _, err := ParseCallLinkJoinAck(linkAck("link_join")); err == nil {
		t.Error("a join ACK with neither group_info nor waiting_room must be rejected")
	}
}

// Identidades divergentes entre a sala e o roster indicam contextos diferentes.
func TestParseCallLinkJoinAckRejectsIdentityMismatch(t *testing.T) {
	room := waitingRoomNode()
	room.Attrs["call-id"] = "OTHER"
	if _, err := ParseCallLinkJoinAck(linkAck("link_join", room, groupInfoNode())); err == nil {
		t.Error("a call-id mismatch must be rejected")
	}
}

func TestParseWaitingRoomUpdate(t *testing.T) {
	node := &waBinary.Node{
		Tag:   "waiting_room_update",
		Attrs: waBinary.Attrs{"call-id": "CALL1", "call-creator": jid("5511999990000")},
		Content: []waBinary.Node{waitingRoomNode(
			waBinary.Node{Tag: "user", Attrs: waBinary.Attrs{"jid": lid("111"), "state": "pending"}},
			waBinary.Node{Tag: "user", Attrs: waBinary.Attrs{"jid": lid("222"), "state": "admitted"}},
		)},
	}
	got, err := ParseWaitingRoomUpdate(node)
	if err != nil {
		t.Fatalf("ParseWaitingRoomUpdate: %v", err)
	}
	if !got.Enabled || !got.IsAdmin {
		t.Errorf("room = %+v", got)
	}
	// O transaction-id ordena os snapshots; quem consome descarta o mais velho.
	if got.TransactionID != 4 {
		t.Errorf("transactionID = %d, want 4", got.TransactionID)
	}
	if len(got.Users) != 2 {
		t.Fatalf("users = %d, want 2", len(got.Users))
	}
	if got.Users[0].State != "pending" || got.Users[1].State != "admitted" {
		t.Errorf("user states = %q / %q", got.Users[0].State, got.Users[1].State)
	}
}

func TestParseWaitingRoomUpdateRejectsInvalid(t *testing.T) {
	mismatched := waitingRoomNode()
	mismatched.Attrs["call-id"] = "OTHER"
	cases := []struct {
		name string
		node *waBinary.Node
	}{
		{"nil", nil},
		{"wrong tag", &waBinary.Node{Tag: "group_update"}},
		{"missing waiting_room", &waBinary.Node{
			Tag:   "waiting_room_update",
			Attrs: waBinary.Attrs{"call-id": "CALL1", "call-creator": jid("1")},
		}},
		{"identity mismatch", &waBinary.Node{
			Tag:     "waiting_room_update",
			Attrs:   waBinary.Attrs{"call-id": "CALL1", "call-creator": jid("5511999990000")},
			Content: []waBinary.Node{mismatched},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseWaitingRoomUpdate(tc.node); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// Um transaction-id nao numerico nao pode virar zero em silencio: zero e um id
// valido e ordenaria errado.
func TestParseWaitingRoomRejectsNonNumericTransactionID(t *testing.T) {
	room := waitingRoomNode()
	room.Attrs["transaction-id"] = "abc"
	node := &waBinary.Node{
		Tag:     "waiting_room_update",
		Attrs:   waBinary.Attrs{"call-id": "CALL1", "call-creator": jid("5511999990000")},
		Content: []waBinary.Node{room},
	}
	if _, err := ParseWaitingRoomUpdate(node); err == nil {
		t.Error("a non-numeric transaction id must be rejected")
	}
}

func TestWaitingRoomBuildersAddressTheCall(t *testing.T) {
	creator := jid("5511999990000")
	user := lid("111")
	cases := []struct {
		name string
		node waBinary.Node
		tag  string
	}{
		{"toggle", BuildWaitingRoomToggle("CALL1", creator, true, "REQ1"), "waiting_room_toggle"},
		{"heartbeat", BuildWaitingRoomHeartbeat("CALL1", creator, "REQ1"), "heartbeat"},
		{"admit", BuildWaitingRoomAdmit("CALL1", creator, user, "REQ1"), "waiting_room_admit"},
		{"deny", BuildWaitingRoomDeny("CALL1", creator, user, "REQ1"), "waiting_room_deny"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.node.Attrs["id"] != "REQ1" {
				t.Errorf("id = %v, want REQ1", tc.node.Attrs["id"])
			}
			to, _ := tc.node.Attrs["to"].(types.JID)
			if to.User != "CALL1" || to.Server != "call" {
				t.Errorf("to = %v, want CALL1@call", to)
			}
			action := tc.node.GetChildren()[0]
			if action.Tag != tc.tag {
				t.Errorf("action = %q, want %q", action.Tag, tc.tag)
			}
			if action.Attrs["call-creator"] != creator {
				t.Errorf("call-creator = %v", action.Attrs["call-creator"])
			}
		})
	}
}

func TestBuildWaitingRoomToggleEncodesEnabled(t *testing.T) {
	onNode := BuildWaitingRoomToggle("CALL1", jid("1"), true, "REQ1")
	offNode := BuildWaitingRoomToggle("CALL1", jid("1"), false, "REQ1")
	on, off := onNode.GetChildren()[0], offNode.GetChildren()[0]
	if on.Attrs["enabled"] != "1" || off.Attrs["enabled"] != "0" {
		t.Errorf("enabled = %v / %v, want 1 / 0", on.Attrs["enabled"], off.Attrs["enabled"])
	}
}

func TestBuildWaitingRoomAdmitCarriesTheUser(t *testing.T) {
	user := lid("111")
	admit := BuildWaitingRoomAdmit("CALL1", jid("1"), user, "REQ1")
	action := admit.GetChildren()[0]
	children := action.GetChildren()
	if len(children) != 1 || children[0].Tag != "user" {
		t.Fatalf("children = %v, want one user", children)
	}
	if children[0].Attrs["jid"] != user {
		t.Errorf("user jid = %v, want %v", children[0].Attrs["jid"], user)
	}
}

func TestBuildWaitingRoomUpdateAck(t *testing.T) {
	from := jid("5511999990000")
	node := &waBinary.Node{
		Tag:   "call",
		Attrs: waBinary.Attrs{"id": "S1", "from": from},
		Content: []waBinary.Node{
			{Tag: "waiting_room_update", Attrs: waBinary.Attrs{"call-id": "CALL1"}},
		},
	}
	ack, err := BuildWaitingRoomUpdateAck(node)
	if err != nil {
		t.Fatalf("BuildWaitingRoomUpdateAck: %v", err)
	}
	if ack.Attrs["type"] != "waiting_room_update" {
		t.Errorf("type = %v, want waiting_room_update", ack.Attrs["type"])
	}
	if ack.Attrs["to"] != from || ack.Attrs["id"] != "S1" {
		t.Errorf("routing = %v / %v", ack.Attrs["to"], ack.Attrs["id"])
	}
}

func TestBuildWaitingRoomUpdateAckRejectsWrongNode(t *testing.T) {
	cases := []struct {
		name string
		node *waBinary.Node
	}{
		{"nil", nil},
		{"not a call", &waBinary.Node{Tag: "ack"}},
		{"wrong action", &waBinary.Node{
			Tag:     "call",
			Attrs:   waBinary.Attrs{"id": "S1", "from": jid("1")},
			Content: []waBinary.Node{{Tag: "group_update"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildWaitingRoomUpdateAck(tc.node); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
