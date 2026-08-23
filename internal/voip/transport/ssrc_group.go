package transport

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"wacalls/internal/voip/media"
)

// RelayStreamSlotWords is the slot order of the nine relay streams the allocate
// advertises. Index 8 carries slot word 6, which is also the app-data slot: see
// PrepareRelayStreamSSRCs for why the three auxiliary slots are not derived.
var RelayStreamSlotWords = [9]uint32{0, 1, 4, 2, 3, 5, 7, 8, 6}

// Hop-by-hop FEC slots, advertised only once the relay switches to
// multi-participant mode.
const (
	HBHFECTXSlotWord uint32 = 7
	HBHFECRXSlotWord uint32 = 8
)

const auxiliarySSRCAttempts = 64

// DeriveRelayStreamSSRCs derives the nine relay-stream SSRCs in slot order, the
// same way both sides derive each other's.
func DeriveRelayStreamSSRCs(callID, deviceJID string) [9]uint32 {
	var out [9]uint32
	for i, slot := range RelayStreamSlotWords {
		out[i] = media.GenerateSecureSsrc(callID, deviceJID, slot)
	}
	return out
}

// PrepareRelayStreamSSRCs replaces the three auxiliary (secondary-video) slots
// with random values. Deriving them would make index 8 equal the app-data SSRC,
// advertising one stream in two subscription groups, which the captured client
// never does.
func PrepareRelayStreamSSRCs(streamSSRCs [9]uint32, appDataSSRC uint32, random io.Reader) ([9]uint32, error) {
	if random == nil {
		return [9]uint32{}, errors.New("transport: relay stream SSRC random source is nil")
	}
	used := make(map[uint32]struct{}, len(streamSSRCs)+1)
	for _, ssrc := range streamSSRCs[:6] {
		if ssrc != 0 {
			used[ssrc] = struct{}{}
		}
	}
	if appDataSSRC != 0 {
		used[appDataSSRC] = struct{}{}
	}
	for i := 6; i < len(streamSSRCs); i++ {
		var selected bool
		for range auxiliarySSRCAttempts {
			var encoded [4]byte
			if _, err := io.ReadFull(random, encoded[:]); err != nil {
				return [9]uint32{}, fmt.Errorf("transport: generate auxiliary video SSRC: %w", err)
			}
			candidate := binary.LittleEndian.Uint32(encoded[:])
			if candidate == 0 {
				continue
			}
			if _, exists := used[candidate]; exists {
				continue
			}
			streamSSRCs[i] = candidate
			used[candidate] = struct{}{}
			selected = true
			break
		}
		if !selected {
			return [9]uint32{}, errors.New("transport: could not generate a unique auxiliary video SSRC")
		}
	}
	return streamSSRCs, nil
}
