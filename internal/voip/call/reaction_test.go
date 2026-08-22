package call

import (
	"log/slog"
	"testing"
	"time"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/engine"
	"wacalls/internal/voip/extension/appdata"
	"wacalls/internal/voip/wanode"
)

// Roundtrip completo: a reaction sai de um CallManager, atravessa SRTP e o relay,
// e chega ao outro pelo hook OnReaction.
func TestReactionFlowsBetweenCallManagers(t *testing.T) {
	k1, k2 := km(1), km(9)

	// Os JIDs sao invertidos entre os dois lados de proposito: o SSRC de app-data
	// e derivado do OwnDeviceJID, e se ambos usassem o mesmo JID os dois lados
	// derivariam o MESMO SSRC. O receptor entao veria o pacote como eco do proprio
	// stream (declaredSelf) e o descartaria antes do handler.
	recv := NewCallManager(fakeSock{}, slog.Default(), appdata.New())
	recv.relay = &fakeRelay{}
	recv.srtp = engine.NewSrtpManager(k2, k1, core.SRTPRecvAuthTagLen, core.SRTPSendAuthTagLen)
	recv.selfSsrc = 2000
	recv.currentCall = &CallInfo{CallID: "CALL1"}
	var gotID, gotEmoji string
	recv.OnReaction = func(callID, emoji string) { gotID, gotEmoji = callID, emoji }
	recv.ensureExtensionsAttachedLocked("peer:0@lid", "our:0@lid")
	defer recv.cleanupMedia()

	send := NewCallManager(fakeSock{}, slog.Default(), appdata.New())
	send.relay = &fakeRelay{onData: recv.onRelayData}
	send.srtp = engine.NewSrtpManager(k1, k2, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	send.selfSsrc = 1000
	send.currentCall = &CallInfo{CallID: "CALL1"}
	send.ensureExtensionsAttachedLocked("our:0@lid", "peer:0@lid")
	// Sem isto a goroutine de retransmissao segue viva por ~450 ms depois do teste.
	defer send.cleanupMedia()

	if err := send.SendReaction("\U0001F44D"); err != nil {
		t.Fatalf("SendReaction: %v", err)
	}
	// A primeira copia sai sincrona, entao o callback ja rodou aqui.
	if gotEmoji != "\U0001F44D" {
		t.Fatalf("received %q, want the thumbs up sent by the peer", gotEmoji)
	}
	if gotID != "CALL1" {
		t.Errorf("callID = %q, want CALL1", gotID)
	}
}

func TestSendReactionWithoutActiveCallFails(t *testing.T) {
	m := NewCallManager(fakeSock{}, slog.Default(), appdata.New())
	if err := m.SendReaction("\U0001F44D"); err == nil {
		t.Fatal("SendReaction without an active call must fail")
	}
}

// Cada reaction gera 10 pacotes; sem limite, clique repetido satura o uplink do
// relay e compete com o audio.
func TestSendReactionIsRateLimited(t *testing.T) {
	k1, k2 := km(1), km(9)
	m := NewCallManager(fakeSock{}, slog.Default(), appdata.New())
	m.relay = &fakeRelay{}
	m.srtp = engine.NewSrtpManager(k1, k2, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	m.currentCall = &CallInfo{CallID: "CALL1"}
	m.ensureExtensionsAttachedLocked("our:0@lid", "peer:0@lid")
	defer m.cleanupMedia()

	if err := m.SendReaction("\U0001F44D"); err != nil {
		t.Fatalf("first reaction: %v", err)
	}
	if err := m.SendReaction("❤️"); err == nil {
		t.Fatal("a second reaction inside the window must be rejected")
	}
	m.mu.Lock()
	m.lastReactionAt = time.Now().Add(-reactionMinInterval * 2)
	m.mu.Unlock()
	if err := m.SendReaction("❤️"); err != nil {
		t.Fatalf("reaction after the window: %v", err)
	}
}

// Depois do cleanup o peer reinicia a sessao de midia e o contador dele volta a
// 1; sem reset, toda reaction nova cairia abaixo da marca e sumiria em silencio.
func TestCleanupMediaResetsReactionDedup(t *testing.T) {
	k1, k2 := km(1), km(9)
	m := NewCallManager(fakeSock{}, slog.Default(), appdata.New())
	m.relay = &fakeRelay{}
	m.srtp = engine.NewSrtpManager(k1, k2, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	m.currentCall = &CallInfo{CallID: "CALL1"}
	m.ensureExtensionsAttachedLocked("our:0@lid", "peer:0@lid")

	sink, ok := engine.Capability[core.ReactionSink](m.extensions)
	if !ok {
		t.Fatal("reaction sink must be discoverable through the extension list")
	}
	ext, ok := sink.(*appdata.AppData)
	if !ok {
		t.Fatal("expected the appdata extension")
	}

	if !ext.AcceptsTransactionID(100) {
		t.Fatal("a fresh dedup must accept any id")
	}
	if ext.AcceptsTransactionID(1) {
		t.Fatal("an id below the mark must be rejected before the reset")
	}

	m.cleanupMedia()

	if !ext.AcceptsTransactionID(1) {
		t.Fatal("cleanupMedia must reset the dedup high-water mark")
	}
}

func TestReinitSrtpResetsReactionDedup(t *testing.T) {
	k1, k2 := km(1), km(9)
	m := NewCallManager(fakeSock{}, slog.Default(), appdata.New())
	m.relay = &fakeRelay{}
	m.srtp = engine.NewSrtpManager(k1, k2, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	m.currentCall = &CallInfo{
		CallID:        "CALL1",
		EncryptionKey: []byte("0123456789abcdef0123456789abcdef"),
	}
	m.ensureExtensionsAttachedLocked("our:0@lid", "peer:0@lid")
	defer m.cleanupMedia()

	sink, _ := engine.Capability[core.ReactionSink](m.extensions)
	ext := sink.(*appdata.AppData)
	ext.AcceptsTransactionID(100)

	m.mu.Lock()
	m.reinitSrtpLocked([]byte("fedcba9876543210fedcba9876543210"), wanode.MustJID("peer:0@lid"))
	m.mu.Unlock()

	if !ext.AcceptsTransactionID(1) {
		t.Fatal("the rekey path must reset the dedup high-water mark")
	}
}
