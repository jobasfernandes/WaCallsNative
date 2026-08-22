package mlow

// Reasons a decoded TOC keeps a frame out of decodeActiveFrame. They double as
// metric labels, so callers can tell a benign DTX frame apart from an operating
// point this decoder does not implement.
const (
	OffPointStdOpus    = "std_opus"
	OffPointInactive   = "inactive"
	OffPointSampleRate = "sample_rate"
	OffPointLowRate    = "low_rate"
	OffPointFrameMs    = "frame_ms"
)

// OffOperatingPointReason reports why a frame will be silenced instead of
// decoded, or "" when it decodes. decodeFrame is the only caller that matters
// for behaviour; instrumentation shares it so a counter can never drift from
// the guard it is supposed to describe.
//
// Order matches the guard's evaluation order, so a frame that is off point for
// more than one reason reports the first one the decoder would hit.
func OffOperatingPointReason(toc SmplTOC) string {
	switch {
	case toc.StdOpus:
		return OffPointStdOpus
	case !toc.Active:
		return OffPointInactive
	case toc.SampleRate != 16000:
		return OffPointSampleRate
	case toc.Flag2:
		return OffPointLowRate
	case toc.FrameMs != 60:
		return OffPointFrameMs
	default:
		return ""
	}
}
