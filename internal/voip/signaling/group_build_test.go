package signaling

import (
	"bytes"
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

func rosterOf(n int) []GroupCallParticipant {
	out := make([]GroupCallParticipant, n)
	for i := range out {
		user := string(rune('a' + i))
		out[i] = GroupCallParticipant{
			JID:   lid(user),
			State: "connected",
			Devices: []GroupCallDevice{{
				JID:               lid(user),
				CapabilityVersion: 1,
				Capability:        []byte{0x01, 0x05, 0xf7, 0x09, 0xe4, 0xbb, 0x13},
			}},
		}
	}
	return out
}

func childTags(node waBinary.Node) []string {
	children := node.GetChildren()
	tags := make([]string, len(children))
	for i, c := range children {
		tags[i] = c.Tag
	}
	return tags
}

// A ordem dos filhos do <offer> e load-bearing: o servidor devolve 439 se estiver
// errada. Este teste existe para quebrar quando alguem reordenar por estetica.
func TestBuildInitialGroupOfferChildOrder(t *testing.T) {
	node, err := BuildInitialGroupOffer(InitialGroupOfferParams{
		CallID:       "CALL1",
		CallCreator:  lid("a"),
		Participants: rosterOf(3),
	})
	if err != nil {
		t.Fatalf("BuildInitialGroupOffer: %v", err)
	}
	offer := node.GetChildren()[0]
	if offer.Tag != "offer" {
		t.Fatalf("action = %q, want offer", offer.Tag)
	}
	want := []string{"audio", "audio", "net", "group_info"}
	got := childTags(offer)
	if len(got) != len(want) {
		t.Fatalf("offer children = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("offer children = %v, want %v", got, want)
		}
	}
	// As duas taxas de audio saem em ordem crescente, como na captura.
	audios := offer.GetChildren()[:2]
	if audios[0].Attrs["rate"] != "8000" || audios[1].Attrs["rate"] != "16000" {
		t.Errorf("audio rates = %v / %v, want 8000 then 16000",
			audios[0].Attrs["rate"], audios[1].Attrs["rate"])
	}
}

// A offer vai para o JID de servico "call", nao para um device.
func TestBuildInitialGroupOfferAddressesTheCallService(t *testing.T) {
	node, err := BuildInitialGroupOffer(InitialGroupOfferParams{
		CallID: "CALL1", CallCreator: lid("a"), Participants: rosterOf(3),
	})
	if err != nil {
		t.Fatalf("BuildInitialGroupOffer: %v", err)
	}
	to, _ := node.Attrs["to"].(types.JID)
	if to.User != "CALL1" || to.Server != "call" {
		t.Errorf("to = %v, want CALL1@call", to)
	}
}

// O group-jid so aparece numa chamada vinculada a um grupo; ad-hoc nao tem.
func TestBuildInitialGroupOfferGroupJIDIsOptional(t *testing.T) {
	adhoc, err := BuildInitialGroupOffer(InitialGroupOfferParams{
		CallID: "CALL1", CallCreator: lid("a"), Participants: rosterOf(3),
	})
	if err != nil {
		t.Fatalf("BuildInitialGroupOffer: %v", err)
	}
	if _, present := adhoc.GetChildren()[0].Attrs["group-jid"]; present {
		t.Error("an ad-hoc group call must not carry group-jid")
	}

	groupJID := types.JID{User: "120363", Server: types.GroupServer}
	bound, err := BuildInitialGroupOffer(InitialGroupOfferParams{
		CallID: "CALL1", CallCreator: lid("a"), GroupJID: groupJID, Participants: rosterOf(3),
	})
	if err != nil {
		t.Fatalf("BuildInitialGroupOffer: %v", err)
	}
	if bound.GetChildren()[0].Attrs["group-jid"] != groupJID {
		t.Errorf("group-jid = %v, want %v", bound.GetChildren()[0].Attrs["group-jid"], groupJID)
	}
}

// A captura nunca abre uma chamada de grupo com menos de tres participantes.
func TestBuildInitialGroupOfferRequiresThreeParticipants(t *testing.T) {
	for n := range 3 {
		if _, err := BuildInitialGroupOffer(InitialGroupOfferParams{
			CallID: "CALL1", CallCreator: lid("a"), Participants: rosterOf(n),
		}); err == nil {
			t.Errorf("a roster of %d must be rejected", n)
		}
	}
}

// A capability de cada device e ecoada exatamente como o roster reportou: um byte
// trocado invalida a oferta.
func TestBuildGroupUsersEchoesCapabilityVerbatim(t *testing.T) {
	odd := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x11, 0x22}
	roster := rosterOf(3)
	roster[0].Devices[0].Capability = odd

	node, err := BuildInitialGroupOffer(InitialGroupOfferParams{
		CallID: "CALL1", CallCreator: lid("a"), Participants: roster,
	})
	if err != nil {
		t.Fatalf("BuildInitialGroupOffer: %v", err)
	}
	groupInfo := node.GetChildren()[0].GetChildren()[3]
	device := groupInfo.GetChildren()[0].GetChildren()[0]
	capability := device.GetChildren()[0]
	got, _ := capability.Content.([]byte)
	if !bytes.Equal(got, odd) {
		t.Errorf("capability = % x, want % x echoed verbatim", got, odd)
	}
	if capability.Attrs["ver"] != "1" {
		t.Errorf("capability ver = %v, want 1", capability.Attrs["ver"])
	}
}

// Um device sem capability nao emite o no: a ausencia e significativa.
func TestBuildGroupUsersOmitsAbsentCapability(t *testing.T) {
	roster := rosterOf(3)
	roster[0].Devices[0].Capability = nil

	node, err := BuildInitialGroupOffer(InitialGroupOfferParams{
		CallID: "CALL1", CallCreator: lid("a"), Participants: roster,
	})
	if err != nil {
		t.Fatalf("BuildInitialGroupOffer: %v", err)
	}
	groupInfo := node.GetChildren()[0].GetChildren()[3]
	device := groupInfo.GetChildren()[0].GetChildren()[0]
	if len(device.GetChildren()) != 0 {
		t.Errorf("device children = %v, want none", childTags(device))
	}
}

func TestBuildGroupUsersRejectsIncompleteRoster(t *testing.T) {
	cases := []struct {
		name  string
		build func() []GroupCallParticipant
	}{
		{"participant without JID", func() []GroupCallParticipant {
			r := rosterOf(3)
			r[1].JID = types.EmptyJID
			return r
		}},
		{"participant without devices", func() []GroupCallParticipant {
			r := rosterOf(3)
			r[1].Devices = nil
			return r
		}},
		{"device without JID", func() []GroupCallParticipant {
			r := rosterOf(3)
			r[1].Devices[0].JID = types.EmptyJID
			return r
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildInitialGroupOffer(InitialGroupOfferParams{
				CallID: "CALL1", CallCreator: lid("a"), Participants: tc.build(),
			}); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestBuildGroupInviteOffer(t *testing.T) {
	targets := []types.JID{lid("b"), lid("c")}
	node, err := BuildGroupInviteOffer(GroupInviteOfferParams{
		CallID: "CALL1", To: lid("b"), CallCreator: lid("a"),
		TargetDevices: targets, Participants: rosterOf(2),
	})
	if err != nil {
		t.Fatalf("BuildGroupInviteOffer: %v", err)
	}
	offer := node.GetChildren()[0]
	want := []string{"audio", "net", "destination", "group_info"}
	got := childTags(offer)
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("offer children = %v, want %v", got, want)
		}
	}
	// O <destination> lista os devices que devem tocar.
	destination := offer.GetChildren()[2]
	if len(destination.GetChildren()) != len(targets) {
		t.Fatalf("destination entries = %d, want %d", len(destination.GetChildren()), len(targets))
	}
	for i, to := range destination.GetChildren() {
		if to.Attrs["jid"] != targets[i] {
			t.Errorf("destination %d = %v, want %v", i, to.Attrs["jid"], targets[i])
		}
	}
}

func TestBuildGroupInviteOfferRejectsIncomplete(t *testing.T) {
	valid := GroupInviteOfferParams{
		CallID: "CALL1", To: lid("b"), CallCreator: lid("a"),
		TargetDevices: []types.JID{lid("b")}, Participants: rosterOf(1),
	}
	cases := map[string]func(*GroupInviteOfferParams){
		"no call ID":      func(p *GroupInviteOfferParams) { p.CallID = "" },
		"no target":       func(p *GroupInviteOfferParams) { p.To = types.EmptyJID },
		"no creator":      func(p *GroupInviteOfferParams) { p.CallCreator = types.EmptyJID },
		"no devices":      func(p *GroupInviteOfferParams) { p.TargetDevices = nil },
		"no participants": func(p *GroupInviteOfferParams) { p.Participants = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			params := valid
			mutate(&params)
			if _, err := BuildGroupInviteOffer(params); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// O preaccept e o accept de grupo usam o requestID dado: a resposta e
// correlacionada por ele.
func TestBuildActiveGroupPreacceptAndAccept(t *testing.T) {
	pre, err := BuildActiveGroupPreaccept("CALL1", lid("a"), "REQ1")
	if err != nil {
		t.Fatalf("BuildActiveGroupPreaccept: %v", err)
	}
	if pre.Attrs["id"] != "REQ1" {
		t.Errorf("preaccept id = %v, want REQ1", pre.Attrs["id"])
	}
	if tag := pre.GetChildren()[0].Tag; tag != "preaccept" {
		t.Errorf("action = %q, want preaccept", tag)
	}

	acc, err := BuildActiveGroupAccept("CALL1", lid("a"), "REQ2")
	if err != nil {
		t.Fatalf("BuildActiveGroupAccept: %v", err)
	}
	if acc.Attrs["id"] != "REQ2" {
		t.Errorf("accept id = %v, want REQ2", acc.Attrs["id"])
	}
	action := acc.GetChildren()[0]
	if action.Tag != "accept" {
		t.Errorf("action = %q, want accept", action.Tag)
	}
	// O accept de grupo nao leva a chave da chamada: ela vem por enc_rekey.
	for _, child := range action.GetChildren() {
		if child.Tag == "enc" {
			t.Error("a group accept must not carry an encrypted call key")
		}
	}
	// Usa a capability deste repo, validada em campo, nao a do upstream.
	for _, child := range action.GetChildren() {
		if child.Tag != "capability" {
			continue
		}
		got, _ := child.Content.([]byte)
		if !bytes.Equal(got, capabilityOffer) {
			t.Errorf("capability = % x, want % x", got, capabilityOffer)
		}
	}
}

func TestBuildActiveGroupPreacceptRejectsIncomplete(t *testing.T) {
	if _, err := BuildActiveGroupPreaccept("", lid("a"), "REQ"); err == nil {
		t.Error("empty call ID must be rejected")
	}
	if _, err := BuildActiveGroupAccept("CALL1", types.EmptyJID, "REQ"); err == nil {
		t.Error("empty creator must be rejected")
	}
	if _, err := BuildActiveGroupAccept("CALL1", lid("a"), ""); err == nil {
		t.Error("empty request ID must be rejected: the response is correlated by it")
	}
}

func TestBuildGroupEncRekey(t *testing.T) {
	to := lid("b")
	node, err := BuildGroupEncRekey(GroupEncRekeyParams{
		CallID: "CALL1", To: to, CallCreator: lid("a"),
		TransactionID: 9, RequestID: "REQ1",
		DeviceKey: GroupOfferDeviceKey{DeviceJID: to, EncType: "pkmsg", Ciphertext: []byte{0x01, 0x02}},
	})
	if err != nil {
		t.Fatalf("BuildGroupEncRekey: %v", err)
	}
	action := node.GetChildren()[0]
	if action.Tag != "enc_rekey" {
		t.Fatalf("action = %q, want enc_rekey", action.Tag)
	}
	if action.Attrs["transaction-id"] != "9" {
		t.Errorf("transaction-id = %v, want 9", action.Attrs["transaction-id"])
	}
	encopt := action.GetChildren()[0]
	if encopt.Attrs["keygen"] != "2" {
		t.Errorf("keygen = %v, want 2", encopt.Attrs["keygen"])
	}
	enc := action.GetChildren()[1]
	if enc.Attrs["v"] != "2" || enc.Attrs["type"] != "pkmsg" {
		t.Errorf("enc attrs = %v", enc.Attrs)
	}
	got, _ := enc.Content.([]byte)
	if !bytes.Equal(got, []byte{0x01, 0x02}) {
		t.Errorf("ciphertext = % x", got)
	}
}

// A chave cifrada tem de ser do device de destino: enviar a de outro entrega uma
// epoch que o destinatario nao consegue decifrar, e o media plane nunca sobe.
func TestBuildGroupEncRekeyRejectsDeviceMismatch(t *testing.T) {
	_, err := BuildGroupEncRekey(GroupEncRekeyParams{
		CallID: "CALL1", To: lid("b"), CallCreator: lid("a"),
		TransactionID: 9, RequestID: "REQ1",
		DeviceKey: GroupOfferDeviceKey{DeviceJID: lid("c"), EncType: "pkmsg", Ciphertext: []byte{0x01}},
	})
	if err == nil {
		t.Error("a ciphertext encrypted for another device must be rejected")
	}
}

func TestBuildGroupEncRekeyRejectsUnsupportedEncType(t *testing.T) {
	to := lid("b")
	_, err := BuildGroupEncRekey(GroupEncRekeyParams{
		CallID: "CALL1", To: to, CallCreator: lid("a"),
		TransactionID: 9, RequestID: "REQ1",
		DeviceKey: GroupOfferDeviceKey{DeviceJID: to, EncType: "other", Ciphertext: []byte{0x01}},
	})
	if err == nil {
		t.Error("an unsupported encryption type must be rejected")
	}
}
