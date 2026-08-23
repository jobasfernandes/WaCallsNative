package signaling

import (
	"bytes"
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

func jid(user string) types.JID { return types.NewJID(user, types.DefaultUserServer) }

func lid(user string) types.JID {
	return types.JID{User: user, Device: 0, Server: types.HiddenUserServer}
}

// groupInfoNode monta um <group_info> com dois participantes, um deles com device
// e capability, no formato que o servidor manda.
func groupInfoNode() waBinary.Node {
	return waBinary.Node{
		Tag: "group_info",
		Attrs: waBinary.Attrs{
			"call-id": "CALL1", "call-creator": jid("5511999990000"),
			"group-jid": types.JID{User: "120363", Server: types.GroupServer},
			"media":     "audio", "transaction-id": "7", "connected-limit": "32",
			"joinable": "1",
		},
		Content: []waBinary.Node{
			{
				Tag:   "user",
				Attrs: waBinary.Attrs{"jid": lid("111"), "state": "connected", "type": "admin"},
				Content: []waBinary.Node{
					{
						Tag:   "device",
						Attrs: waBinary.Attrs{"jid": lid("111"), "platform": "web", "pid": "1"},
						Content: []waBinary.Node{
							{
								Tag:     "capability",
								Attrs:   waBinary.Attrs{"ver": "1"},
								Content: []byte{0x01, 0x05, 0xf7, 0x09, 0xe0, 0xbb, 0x13},
							},
						},
					},
				},
			},
			{
				Tag:   "user",
				Attrs: waBinary.Attrs{"jid": lid("222"), "state": "pending"},
			},
		},
	}
}

func TestParseGroupUpdate(t *testing.T) {
	node := &waBinary.Node{
		Tag:   "group_update",
		Attrs: waBinary.Attrs{"call-id": "CALL1", "call-creator": jid("5511999990000")},
		Content: []waBinary.Node{
			groupInfoNode(),
			{Tag: "av_upgrade", Attrs: waBinary.Attrs{"av-upgradable": "1"}},
		},
	}
	got, err := ParseGroupUpdate(node)
	if err != nil {
		t.Fatalf("ParseGroupUpdate: %v", err)
	}
	if got.CallID != "CALL1" {
		t.Errorf("callID = %q, want CALL1", got.CallID)
	}
	// O transaction-id ordena os snapshots: um update mais velho tem de ser
	// descartavel por quem consome, entao o parser tem de expor o numero.
	if got.TransactionID != 7 {
		t.Errorf("transactionID = %d, want 7", got.TransactionID)
	}
	if got.ConnectedLimit != 32 {
		t.Errorf("connectedLimit = %d, want 32", got.ConnectedLimit)
	}
	if !got.Joinable || !got.AVUpgradable {
		t.Errorf("joinable = %v, avUpgradable = %v, want both true", got.Joinable, got.AVUpgradable)
	}
	if len(got.Participants) != 2 {
		t.Fatalf("participants = %d, want 2", len(got.Participants))
	}
	if got.Participants[0].State != "connected" || got.Participants[0].Type != "admin" {
		t.Errorf("participant 0 = %+v, want connected/admin", got.Participants[0])
	}
	if len(got.Participants[0].Devices) != 1 {
		t.Fatalf("participant 0 devices = %d, want 1", len(got.Participants[0].Devices))
	}
	device := got.Participants[0].Devices[0]
	if !device.HasPID || device.PID != 1 {
		t.Errorf("device PID = %d (has=%v), want 1", device.PID, device.HasPID)
	}
	// A capability e ecoada de volta no accept: um byte trocado invalida a oferta.
	wantCap := []byte{0x01, 0x05, 0xf7, 0x09, 0xe0, 0xbb, 0x13}
	if !bytes.Equal(device.Capability, wantCap) {
		t.Errorf("capability = % x, want % x", device.Capability, wantCap)
	}
	if device.CapabilityVersion != 1 {
		t.Errorf("capability version = %d, want 1", device.CapabilityVersion)
	}
	if len(got.Participants[1].Devices) != 0 {
		t.Errorf("participant 1 must have no devices, got %d", len(got.Participants[1].Devices))
	}
}

func TestParseGroupUpdateRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		node *waBinary.Node
	}{
		{"nil", nil},
		{"wrong tag", &waBinary.Node{Tag: "offer"}},
		{"missing group_info", &waBinary.Node{
			Tag:   "group_update",
			Attrs: waBinary.Attrs{"call-id": "C", "call-creator": jid("1")},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseGroupUpdate(tc.node); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// transaction-id e connected-limit sao obrigatorios: sem eles o snapshot nao
// pode ser ordenado nem validado.
func TestParseGroupInfoRequiresTransactionAndLimit(t *testing.T) {
	for _, missing := range []string{"transaction-id", "connected-limit"} {
		t.Run("missing "+missing, func(t *testing.T) {
			info := groupInfoNode()
			delete(info.Attrs, missing)
			node := &waBinary.Node{
				Tag:     "group_update",
				Attrs:   waBinary.Attrs{"call-id": "CALL1", "call-creator": jid("5511999990000")},
				Content: []waBinary.Node{info},
			}
			if _, err := ParseGroupUpdate(node); err == nil {
				t.Errorf("a group_info without %s must be rejected", missing)
			}
		})
	}
}

func TestParseInitialGroupCallAckWithRelay(t *testing.T) {
	node := &waBinary.Node{
		Tag: "ack",
		Content: []waBinary.Node{
			groupInfoNode(),
			{
				Tag: "relay",
				Attrs: waBinary.Attrs{
					"uuid": "RELAY-UUID", "participant_uuid": "PART-UUID",
					"transaction-id": "7", "self_pid": "3", "warp_mi_tag_len": "4",
					"attribute_padding": "1",
				},
				Content: []waBinary.Node{
					{Tag: "key", Content: []byte{0xaa, 0xbb}},
					{Tag: "hbh_key", Content: []byte{0xcc, 0xdd}},
					// Tokens sao enderecados por id, nao por posicao.
					{Tag: "token", Attrs: waBinary.Attrs{"id": "2"}, Content: []byte{0x22}},
					{Tag: "token", Attrs: waBinary.Attrs{"id": "0"}, Content: []byte{0x00}},
					{
						Tag: "te2",
						Attrs: waBinary.Attrs{
							"relay_name": "sao1", "domain_name": "relay.example",
							"relay_id": "5", "token_id": "2", "auth_token_id": "1",
							"c2r_rtt": "23", "is_fna": "1",
						},
						Content: []byte{192, 168, 1, 10, 0x0d, 0x98},
					},
				},
			},
		},
	}
	got, ok, err := ParseInitialGroupCallAck(node)
	if err != nil {
		t.Fatalf("ParseInitialGroupCallAck: %v", err)
	}
	if !ok {
		t.Fatal("an ack carrying group_info must be recognized")
	}
	if got.Relay == nil {
		t.Fatal("relay must be parsed")
	}
	r := got.Relay
	if r.UUID != "RELAY-UUID" || r.ParticipantUUID != "PART-UUID" {
		t.Errorf("uuid = %q / %q", r.UUID, r.ParticipantUUID)
	}
	// self_pid identifica este device no relay: sem ele as subscriptions saem erradas.
	if !r.HasSelfPID || r.SelfPID != 3 {
		t.Errorf("selfPID = %d (has=%v), want 3", r.SelfPID, r.HasSelfPID)
	}
	if !r.HasWarpMITagLength || r.WarpMITagLength != 4 {
		t.Errorf("warpMITagLength = %d (has=%v), want 4", r.WarpMITagLength, r.HasWarpMITagLength)
	}
	if !r.AttributePadding {
		t.Error("attributePadding must be true")
	}
	if !bytes.Equal(r.Key, []byte{0xaa, 0xbb}) || !bytes.Equal(r.HBHKey, []byte{0xcc, 0xdd}) {
		t.Errorf("key = % x, hbhKey = % x", r.Key, r.HBHKey)
	}
	// O token de id 2 tem de cair no indice 2, com buraco no 1.
	if len(r.Tokens) != 3 {
		t.Fatalf("tokens = %d, want 3 (indices 0..2)", len(r.Tokens))
	}
	if !bytes.Equal(r.Tokens[0], []byte{0x00}) || r.Tokens[1] != nil || !bytes.Equal(r.Tokens[2], []byte{0x22}) {
		t.Errorf("tokens = %v, want index 0 and 2 filled and index 1 empty", r.Tokens)
	}
	if len(r.Endpoints) != 1 {
		t.Fatalf("endpoints = %d, want 1", len(r.Endpoints))
	}
	e := r.Endpoints[0]
	if e.RelayName != "sao1" || e.RelayID != 5 || e.TokenID != 2 || e.AuthTokenID != 1 || e.RTT != 23 {
		t.Errorf("endpoint = %+v", e)
	}
	if !e.IsFNA {
		t.Error("is_fna must be parsed: it marks the relay carrying inbound media")
	}
	if e.IPv4 != "192.168.1.10" || e.Port != 3480 {
		t.Errorf("address = %s:%d, want 192.168.1.10:3480", e.IPv4, e.Port)
	}
}

// Um ack sem group_info nao e erro: e um ack de outra coisa.
func TestParseInitialGroupCallAckWithoutGroupInfo(t *testing.T) {
	got, ok, err := ParseInitialGroupCallAck(&waBinary.Node{Tag: "ack"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || got != nil {
		t.Errorf("got %v (ok=%v), want nil and false", got, ok)
	}
}

func TestParseGroupInviteSnapshot(t *testing.T) {
	offer := &waBinary.Node{
		Tag: "offer",
		Attrs: waBinary.Attrs{
			"call-id": "CALL1", "call-creator": jid("5511999990000"), "joinable": "1",
		},
		Content: []waBinary.Node{groupInfoNode()},
	}
	got, ok, err := ParseGroupInviteSnapshot(offer)
	if err != nil {
		t.Fatalf("ParseGroupInviteSnapshot: %v", err)
	}
	if !ok || got == nil {
		t.Fatal("an offer carrying group_info must be recognized")
	}
	if !got.Joinable {
		t.Error("joinable must be read from the offer")
	}
}

// Uma offer 1:1 nao e invite de grupo, e isso nao e erro.
func TestParseGroupInviteSnapshotOnDirectOffer(t *testing.T) {
	offer := &waBinary.Node{
		Tag:   "offer",
		Attrs: waBinary.Attrs{"call-id": "CALL1", "call-creator": jid("5511999990000")},
	}
	got, ok, err := ParseGroupInviteSnapshot(offer)
	if err != nil || ok || got != nil {
		t.Fatalf("got %v (ok=%v, err=%v), want nil, false, nil", got, ok, err)
	}
}

// O call-id do group_info tem de bater com o da offer, senao e outro contexto.
func TestParseGroupInviteSnapshotRejectsMismatch(t *testing.T) {
	info := groupInfoNode()
	info.Attrs["call-id"] = "OTHER"
	offer := &waBinary.Node{
		Tag:     "offer",
		Attrs:   waBinary.Attrs{"call-id": "CALL1", "call-creator": jid("5511999990000")},
		Content: []waBinary.Node{info},
	}
	if _, _, err := ParseGroupInviteSnapshot(offer); err == nil {
		t.Error("a call-id mismatch must be rejected")
	}
}

func TestParseGroupCallEncRekey(t *testing.T) {
	node := &waBinary.Node{
		Tag:   "enc_rekey",
		Attrs: waBinary.Attrs{"call-id": "CALL1", "call-creator": jid("1"), "transaction-id": "9"},
		Content: []waBinary.Node{
			{Tag: "encopt", Attrs: waBinary.Attrs{"keygen": "2"}},
			{Tag: "enc", Attrs: waBinary.Attrs{"type": "pkmsg", "v": "2"}, Content: []byte{0x01, 0x02, 0x03}},
		},
	}
	got, err := ParseGroupCallEncRekey(node)
	if err != nil {
		t.Fatalf("ParseGroupCallEncRekey: %v", err)
	}
	if got.TransactionID != 9 || got.KeyGeneration != 2 || got.EncryptionType != "pkmsg" {
		t.Errorf("rekey = %+v", got)
	}
	if !bytes.Equal(got.Ciphertext, []byte{0x01, 0x02, 0x03}) {
		t.Errorf("ciphertext = % x", got.Ciphertext)
	}
}

// O media plane de grupo so sabe keyar com keygen 2 e envelope Signal v2.
func TestParseGroupCallEncRekeyRejectsUnsupported(t *testing.T) {
	cases := []struct {
		name   string
		keygen string
		encTyp string
		encVer string
	}{
		{"keygen 1", "1", "pkmsg", "2"},
		{"encryption v1", "2", "pkmsg", "1"},
		{"unknown envelope", "2", "other", "2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &waBinary.Node{
				Tag:   "enc_rekey",
				Attrs: waBinary.Attrs{"call-id": "C", "call-creator": jid("1"), "transaction-id": "1"},
				Content: []waBinary.Node{
					{Tag: "encopt", Attrs: waBinary.Attrs{"keygen": tc.keygen}},
					{Tag: "enc", Attrs: waBinary.Attrs{"type": tc.encTyp, "v": tc.encVer}, Content: []byte{0x01}},
				},
			}
			if _, err := ParseGroupCallEncRekey(node); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestParseCallControlEnvelope(t *testing.T) {
	from := jid("5511999990000")
	participant := lid("111")
	node := &waBinary.Node{
		Tag:   "call",
		Attrs: waBinary.Attrs{"from": from, "participant": participant, "id": "S1"},
		Content: []waBinary.Node{
			{
				Tag:   "group_update",
				Attrs: waBinary.Attrs{"call-id": "CALL1", "call-creator": from},
			},
		},
	}
	got, err := ParseCallControlEnvelope(node)
	if err != nil {
		t.Fatalf("ParseCallControlEnvelope: %v", err)
	}
	if got.Action.Tag != "group_update" {
		t.Errorf("action = %q, want group_update", got.Action.Tag)
	}
	if got.From != from || got.Participant != participant {
		t.Errorf("routing = %v / %v", got.From, got.Participant)
	}
	if got.CallID != "CALL1" {
		t.Errorf("callID = %q", got.CallID)
	}
}

// Um no com mais de uma acao nao e um envelope de controle valido.
func TestParseCallControlEnvelopeRejectsMultipleActions(t *testing.T) {
	node := &waBinary.Node{
		Tag:   "call",
		Attrs: waBinary.Attrs{"from": jid("1")},
		Content: []waBinary.Node{
			{Tag: "group_update"}, {Tag: "enc_rekey"},
		},
	}
	if _, err := ParseCallControlEnvelope(node); err == nil {
		t.Error("expected an error for two actions")
	}
}
