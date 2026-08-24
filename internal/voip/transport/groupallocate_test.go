package transport

import (
	"encoding/binary"
	"testing"
)

// attrOrder extrai os tipos de atributo de uma mensagem STUN, na ordem em que
// aparecem.
func attrOrder(t *testing.T, msg []byte) []int {
	t.Helper()
	if len(msg) < 20 {
		t.Fatalf("message too short: %d bytes", len(msg))
	}
	var out []int
	body := msg[20:]
	for len(body) >= 4 {
		typ := int(binary.BigEndian.Uint16(body[0:2]))
		length := int(binary.BigEndian.Uint16(body[2:4]))
		out = append(out, typ)
		padded := length + (4-length%4)%4
		if 4+padded > len(body) {
			break
		}
		body = body[4+padded:]
	}
	return out
}

func groupParams(pids []uint32, hbhFEC [2]uint32) GroupAllocateParams {
	return GroupAllocateParams{
		RelayToken:  []byte{0xAA, 0xBB},
		Streams:     [9]uint32{10, 11, 12, 20, 21, 22, 30, 31, 32},
		AppDataSSRC: 40,
		PIDs:        pids,
		HBHFEC:      hbhFEC,
		HMACKey:     []byte("key"),
		RelayIP:     "192.168.1.10",
		RelayPort:   3480,
	}
}

// A ordem dos atributos e a da captura; o servidor rejeita fora dela.
func TestGroupAllocateAttributeOrder(t *testing.T) {
	msg := BuildGroupAllocate(groupParams([]uint32{1, 2}, [2]uint32{900, 901}))
	want := []int{
		attrRelayToken, attrSenderSubs, attrReceiverSubs,
		attrStreamDescriptors, attrParticipantCount,
		attrXorRelayedAddress, attrMessageIntegrity,
	}
	got := attrOrder(t, msg)
	if len(got) != len(want) {
		t.Fatalf("attributes = %#x, want %#x", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("attributes = %#x, want %#x", got, want)
		}
	}
}

// Os descritores HBH-FEC so entram quando o relay passa a modo multi-participante,
// que a captura mostra ser com mais de um PID remoto.
func TestGroupAllocateHBHFECOnlyWithTwoOrMorePIDs(t *testing.T) {
	one := BuildGroupAllocate(groupParams([]uint32{1}, [2]uint32{900, 901}))
	two := BuildGroupAllocate(groupParams([]uint32{1, 2}, [2]uint32{900, 901}))
	if len(two) <= len(one) {
		t.Fatalf("two PIDs (%d bytes) must carry more than one PID (%d bytes)", len(two), len(one))
	}
	// Sem PID nenhum tambem nao ha FEC.
	none := BuildGroupAllocate(groupParams(nil, [2]uint32{900, 901}))
	if len(none) >= len(one) {
		t.Errorf("no PIDs (%d) must not carry more than one PID (%d)", len(none), len(one))
	}
}

func TestGroupAllocateParticipantCount(t *testing.T) {
	for _, tc := range []struct {
		name string
		pids []uint32
		want byte
	}{
		{"one", []uint32{1}, 1},
		{"two", []uint32{1, 2}, 2},
		{"deduplicated", []uint32{1, 2, 2}, 2},
		{"zero kept", []uint32{0, 1}, 2},
		{"zero alone", []uint32{0}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := BuildGroupAllocate(groupParams(tc.pids, [2]uint32{}))
			body := msg[20:]
			for len(body) >= 4 {
				typ := int(binary.BigEndian.Uint16(body[0:2]))
				length := int(binary.BigEndian.Uint16(body[2:4]))
				if typ == attrParticipantCount {
					if body[4] != tc.want {
						t.Fatalf("participant count = %d, want %d", body[4], tc.want)
					}
					return
				}
				padded := length + (4-length%4)%4
				body = body[4+padded:]
			}
			t.Fatal("participant count attribute not found")
		})
	}
}

// O allocate de grupo continua sendo uma mensagem STUN de allocate valida.
func TestGroupAllocateIsAnAllocateRequest(t *testing.T) {
	msg := BuildGroupAllocate(groupParams([]uint32{1}, [2]uint32{}))
	if typ := int(binary.BigEndian.Uint16(msg[0:2])); typ != stunAllocateRequest {
		t.Errorf("message type = %#x, want %#x", typ, stunAllocateRequest)
	}
	if cookie := binary.BigEndian.Uint32(msg[4:8]); cookie != stunMagicCookie {
		t.Errorf("magic cookie = %#x", cookie)
	}
	if declared, actual := int(binary.BigEndian.Uint16(msg[2:4])), len(msg)-20; declared != actual {
		t.Errorf("declared length %d, actual %d", declared, actual)
	}
}

// Um participante com PID 0 tem de aparecer nas duas direcoes. Sem ele o
// participante fica sem receber a nossa voz e as nossas reacoes, e nos ficamos
// sem receber a dele.
func TestGroupAllocateKeepsParticipantZero(t *testing.T) {
	withZero := BuildGroupAllocate(groupParams([]uint32{0, 1}, [2]uint32{900, 901}))
	onlyOne := BuildGroupAllocate(groupParams([]uint32{1}, [2]uint32{900, 901}))
	if len(withZero) <= len(onlyOne) {
		t.Fatalf("PID 0 must add subscriptions: %d bytes vs %d", len(withZero), len(onlyOne))
	}
	// Dois participantes sao o que poe o relay em modo de encaminhamento, e o
	// HBH-FEC so entra ai.
	body := withZero[20:]
	for len(body) >= 4 {
		typ := int(binary.BigEndian.Uint16(body[0:2]))
		length := int(binary.BigEndian.Uint16(body[2:4]))
		if typ == attrParticipantCount {
			if body[4] != 2 {
				t.Fatalf("participant count = %d, want 2 with PIDs 0 and 1", body[4])
			}
			return
		}
		body = body[4+length+(4-length%4)%4:]
	}
	t.Fatal("participant count attribute not found")
}

// O relay sonda o caminho com um binding request antes de encaminhar midia. Sem
// resposta o caminho de volta fica fechado: continuamos publicando e nao
// recebemos nada.
func TestBindingSuccessAnswersTheRelayProbe(t *testing.T) {
	key := []byte("relaykey")
	request := buildStunMessage(stunBindingRequest, nil,
		[]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, key, true)

	response, ok := BuildBindingSuccess(request, key)
	if !ok {
		t.Fatal("a binding request from the relay must be answered")
	}
	if got := int(binary.BigEndian.Uint16(response[0:2])); got != stunBindingSuccess {
		t.Errorf("message type = %#x, want binding success %#x", got, stunBindingSuccess)
	}
	// O transaction id tem de ser o do pedido, senao o relay nao correlaciona.
	for i := range 12 {
		if response[8+i] != request[8+i] {
			t.Fatalf("transaction id differs at byte %d", i)
		}
	}
	// E tem de vir assinado: o relay valida a integridade com a mesma chave.
	order := attrOrder(t, response)
	if len(order) != 2 || order[0] != attrMessageIntegrity || order[1] != attrFingerprint {
		t.Errorf("attributes = %#x, want message-integrity then fingerprint", order)
	}
}

// Nao respondemos a qualquer coisa: uma resposta so sai para um binding request.
func TestBindingSuccessIgnoresNonRequests(t *testing.T) {
	key := []byte("relaykey")
	allocate := BuildGroupAllocate(groupParams([]uint32{0, 1}, [2]uint32{}))
	if _, ok := BuildBindingSuccess(allocate, key); ok {
		t.Error("an allocate request must not be answered with a binding success")
	}
	if _, ok := BuildBindingSuccess([]byte{1, 2, 3}, key); ok {
		t.Error("a short packet must not be answered")
	}
}

// O ping vai antes do allocate, e nenhum binding request sai: um binding request
// poe o relay em modo de consent ICE e a ponte nunca se forma.
func TestGroupRegistrationSendsPingThenAllocateOnly(t *testing.T) {
	cfg := &GroupAllocateConfig{
		Streams: [9]uint32{10, 11, 12, 20, 21, 22, 30, 31, 32},
		PIDs:    []uint32{0, 1}, Key: []byte("relaykey"),
		Tokens: map[string][]byte{"sao1": []byte("grouptok")},
	}
	info := RelayConfig{
		Name: "sao1", IP: "192.168.1.10", Port: 3480,
		RawToken: []byte("onetoone"), Key: "otherkey",
	}
	packets := GroupRegistrationPackets(cfg, info)
	if len(packets) != 2 {
		t.Fatalf("sent %d packets, want the ping and the allocate only", len(packets))
	}
	if got := binary.BigEndian.Uint16(packets[0][0:2]); got != whatsappPing {
		t.Errorf("first packet = %#x, want the consent ping", got)
	}
	if got := int(binary.BigEndian.Uint16(packets[1][0:2])); got != stunAllocateRequest {
		t.Errorf("second packet = %#x, want the allocate", got)
	}
	for _, p := range packets {
		if binary.BigEndian.Uint16(p[0:2]) == stunBindingRequest {
			t.Error("a group registration must send no binding request")
		}
	}
}
