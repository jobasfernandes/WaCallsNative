package media

import (
	"bytes"
	"encoding/hex"
	"testing"

	"wacalls/internal/voip/core"
)

// KAT vectors from oxidezap/whatsapp-rust@d9e693f wacore/src/voip/testdata/kats.json
// (sections "inputs" and "hbh_srtp"). Inputs are synthetic; matching them proves parity
// with the whatsapp-rust implementation.
const (
	katHbhKey      = "404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d"
	katUplinkMK    = "d7c293861dccfc246ce7c66b498e7d50"
	katUplinkMS    = "e92424ae12c54f79c9131eac0a94"
	katSessionKey  = "8586b2992b154e6531606e8b7ceeb782"
	katSessionSalt = "e14ea5189ca6f303f3697237cf04"
	katAuthKey     = "1caf9c0b1f0c08fad93a41f7d6897ba12eaf100f"
	katPayload     = "102132435465768798a9bacb"
	katCipherOut   = "eacd64249a77b5270140c634"
	katSsrc        = uint32(0x12345678)
	katPacketIndex = uint64(7) // roc=0, seq=7
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func TestDeriveHbhSrtpKeyingUplinkKAT(t *testing.T) {
	keying, err := DeriveHbhSrtpKeying(mustHex(t, katHbhKey), true)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if got := hex.EncodeToString(keying.MasterKey); got != katUplinkMK {
		t.Errorf("master key: got %s want %s", got, katUplinkMK)
	}
	if got := hex.EncodeToString(keying.MasterSalt); got != katUplinkMS {
		t.Errorf("master salt: got %s want %s", got, katUplinkMS)
	}

	ctx, err := NewSrtpContext(keying, core.SRTPAuthTagLen)
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if got := hex.EncodeToString(ctx.sessionKey); got != katSessionKey {
		t.Errorf("session key: got %s want %s", got, katSessionKey)
	}
	if got := hex.EncodeToString(ctx.sessionSalt); got != katSessionSalt {
		t.Errorf("session salt: got %s want %s", got, katSessionSalt)
	}
	if got := hex.EncodeToString(ctx.authKey); got != katAuthKey {
		t.Errorf("auth key: got %s want %s", got, katAuthKey)
	}

	// Cipher-level proof: AES-ICM of the payload at (ssrc, packet index) equals the kat cipher_out.
	iv := ctx.generateIV(katSsrc, katPacketIndex)
	payload := mustHex(t, katPayload)
	out := make([]byte, len(payload))
	if err := aesCtrXor(ctx.sessionKey, iv, payload, out); err != nil {
		t.Fatalf("aes-ctr: %v", err)
	}
	if got := hex.EncodeToString(out); got != katCipherOut {
		t.Errorf("cipher out: got %s want %s", got, katCipherOut)
	}
}

func TestDeriveHbhSrtpKeyingDownlinkDiffersAndDeterministic(t *testing.T) {
	hbh := mustHex(t, katHbhKey)
	up, err := DeriveHbhSrtpKeying(hbh, true)
	if err != nil {
		t.Fatalf("uplink: %v", err)
	}
	down1, err := DeriveHbhSrtpKeying(hbh, false)
	if err != nil {
		t.Fatalf("downlink: %v", err)
	}
	down2, err := DeriveHbhSrtpKeying(hbh, false)
	if err != nil {
		t.Fatalf("downlink 2: %v", err)
	}
	if !bytes.Equal(down1.MasterKey, down2.MasterKey) || !bytes.Equal(down1.MasterSalt, down2.MasterSalt) {
		t.Error("downlink derivation is not deterministic")
	}
	if bytes.Equal(up.MasterKey, down1.MasterKey) {
		t.Error("uplink and downlink master keys must differ")
	}
}

func TestDeriveHbhSrtpKeyingBadLength(t *testing.T) {
	for _, n := range []int{0, 29, 31} {
		if _, err := DeriveHbhSrtpKeying(make([]byte, n), true); err == nil {
			t.Errorf("len %d: expected error, got nil", n)
		}
	}
}
