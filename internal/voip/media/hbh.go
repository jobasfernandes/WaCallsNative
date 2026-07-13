package media

import (
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"

	"wacalls/internal/voip/core"
)

const hbhKeyLen = 30

const (
	hbhUplinkSaltLabel   = "uplink hbh srtcp salt"
	hbhUplinkKeyLabel    = "uplink hbh srtcp key"
	hbhDownlinkSaltLabel = "downlink hbh srtcp salt"
	hbhDownlinkKeyLabel  = "downlink hbh srtcp key"
)

// DeriveHbhSrtpKeying turns the 30-byte relay hbh_key (16B master key + 14B master salt) into
// per-direction SRTP keying material via WhatsApp's two-layer HKDF-SHA256 schedule. uplink is our
// TX direction. The result feeds NewSrtpContext for the libsrtp session-key expansion.
func DeriveHbhSrtpKeying(hbhKey []byte, uplink bool) (core.SrtpKeyingMaterial, error) {
	if len(hbhKey) != hbhKeyLen {
		return core.SrtpKeyingMaterial{}, fmt.Errorf("hbh_key must be %d bytes, got %d", hbhKeyLen, len(hbhKey))
	}
	masterKey := hbhKey[:16]
	masterSalt := hbhKey[16:hbhKeyLen]

	saltLabel, keyLabel := hbhDownlinkSaltLabel, hbhDownlinkKeyLabel
	if uplink {
		saltLabel, keyLabel = hbhUplinkSaltLabel, hbhUplinkKeyLabel
	}

	srtcpSalt, err := hkdf.Key(sha256.New, masterSalt, make([]byte, 32), saltLabel, 32)
	if err != nil {
		return core.SrtpKeyingMaterial{}, fmt.Errorf("derive hbh srtcp salt: %w", err)
	}
	cryptoKey, err := hkdf.Key(sha256.New, masterKey, srtcpSalt, keyLabel, hbhKeyLen)
	if err != nil {
		return core.SrtpKeyingMaterial{}, fmt.Errorf("derive hbh srtcp key: %w", err)
	}

	mk := make([]byte, 16)
	ms := make([]byte, 14)
	copy(mk, cryptoKey[:16])
	copy(ms, cryptoKey[16:hbhKeyLen])
	return core.SrtpKeyingMaterial{MasterKey: mk, MasterSalt: ms}, nil
}
