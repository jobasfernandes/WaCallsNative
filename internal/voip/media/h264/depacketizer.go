// Package h264 reassembles H.264 NAL units from RFC 6184 RTP payloads. WhatsApp video
// (PT 97) arrives H.264-in-RTP; this turns the decrypted RTP payloads into complete NAL
// units ready to re-packetize onto a browser video track.
package h264

import "fmt"

const nalTypeFUA = 28 // RFC 6184 §5.8 Fragmentation Unit A

// Depacketizer reassembles H.264 NAL units from a sequence of RFC 6184 RTP payloads fed in
// arrival order. Partial FU-A fragments are buffered across calls.
type Depacketizer struct {
	fuBuf    []byte
	fuActive bool

	lastSeq      uint16
	seqInit      bool
	waitKeyframe bool

	// KeyframeWaits counts sequence-gap events that dropped to keyframe-wait (diagnostic).
	KeyframeWaits int
}

// Depacketize returns the complete NAL units (no start code) contained in one RTP payload.
// A sequence-number gap drops the partial FU-A and enters keyframe-wait: emitting a NAL that
// references a lost frame smears the browser decoder, so NALs are discarded until the next
// keyframe.
func (d *Depacketizer) Depacketize(seq uint16, payload []byte) ([][]byte, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	if d.seqInit && seq != d.lastSeq+1 {
		d.fuBuf = nil
		d.fuActive = false
		if !d.waitKeyframe {
			d.KeyframeWaits++
		}
		d.waitKeyframe = true
	}
	d.lastSeq = seq
	d.seqInit = true

	nalType := payload[0] & 0x1f
	var nals [][]byte
	switch {
	case nalType >= 1 && nalType <= 23:
		nals = [][]byte{payload}
	case nalType == nalTypeFUA:
		out, err := d.depacketizeFUA(payload)
		if err != nil {
			return nil, err
		}
		nals = out
	default:
		// STAP-A/STAP-B/MTAP and reserved: not sent by WhatsApp's 1:1 video stream.
		return nil, nil
	}
	return d.gateKeyframe(nals), nil
}

func (d *Depacketizer) gateKeyframe(nals [][]byte) [][]byte {
	if !d.waitKeyframe {
		return nals
	}
	var out [][]byte
	for _, nal := range nals {
		if len(nal) == 0 {
			continue
		}
		if t := nal[0] & 0x1f; t == 7 || t == 5 {
			d.waitKeyframe = false
		}
		if !d.waitKeyframe {
			out = append(out, nal)
		}
	}
	return out
}

func (d *Depacketizer) depacketizeFUA(payload []byte) ([][]byte, error) {
	if len(payload) < 2 {
		return nil, fmt.Errorf("h264: FU-A payload too short: %d bytes", len(payload))
	}
	fuIndicator := payload[0]
	fuHeader := payload[1]
	start := fuHeader&0x80 != 0
	end := fuHeader&0x40 != 0
	frag := payload[2:]

	if start {
		// Reconstruct the NAL header: F|NRI from the indicator, type from the FU header.
		nalHeader := (fuIndicator & 0xe0) | (fuHeader & 0x1f)
		d.fuBuf = append([]byte{nalHeader}, frag...)
		d.fuActive = true
	} else {
		if !d.fuActive {
			return nil, nil // orphan fragment (start was lost)
		}
		d.fuBuf = append(d.fuBuf, frag...)
	}

	if end && d.fuActive {
		nal := d.fuBuf
		d.fuBuf = nil
		d.fuActive = false
		return [][]byte{nal}, nil
	}
	return nil, nil
}

var startCode = []byte{0x00, 0x00, 0x00, 0x01}

// IsPlausibleNALHeader reports whether a byte looks like a valid H.264 NAL header
// (forbidden_zero_bit clear and a type WhatsApp actually sends for video). It locks onto the
// video stream among the relay's non-audio flows without relying on a fixed payload type.
func IsPlausibleNALHeader(b byte) bool {
	if b&0x80 != 0 {
		return false
	}
	switch b & 0x1f {
	case 1, 5, 6, 7, 8, 24, 28:
		return true
	}
	return false
}

// AnnexB serializes NAL units into a single Annex-B bytestream, prefixing each with the
// 4-byte start code.
func AnnexB(nals [][]byte) []byte {
	var out []byte
	for _, nal := range nals {
		out = append(out, startCode...)
		out = append(out, nal...)
	}
	return out
}
