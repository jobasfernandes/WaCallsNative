package signaling

import (
	"strconv"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/wanode"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

const (
	VideoDecRequest = "H264"
	VideoDecAccept  = "H264,AV1"
)

type VideoStateParams struct {
	CallID            string
	To                types.JID
	CallCreator       types.JID
	State             int
	Dec               string
	DeviceOrientation *int
}

func BuildVideoStateStanza(p VideoStateParams) waBinary.Node {
	attrs := waBinary.Attrs{
		"call-id":      p.CallID,
		"call-creator": p.CallCreator,
		"state":        strconv.Itoa(p.State),
	}
	if p.Dec != "" {
		attrs["dec"] = p.Dec
	}
	if p.State == core.VideoStateUpgradeRequestV2 {
		attrs["voip_settings"] = "video"
	}
	if p.DeviceOrientation != nil {
		attrs["device_orientation"] = strconv.Itoa(*p.DeviceOrientation)
	}
	return callWrap(p.To, waBinary.Node{Tag: "video", Attrs: attrs})
}

// BuildVideoAck builds the typed ack an inbound <video> node requires; a bare typeless
// ack makes the sender cancel the upgrade after ~5s. Echoes companion-device routing.
func BuildVideoAck(callNode *waBinary.Node) (waBinary.Node, bool) {
	ag := callNode.AttrGetter()
	id := ag.OptionalString("id")
	from := ag.OptionalJIDOrEmpty("from")
	if id == "" || from.IsEmpty() {
		return waBinary.Node{}, false
	}
	attrs := waBinary.Attrs{"class": "call", "id": id, "to": from, "type": "video"}
	if participant := ag.OptionalJIDOrEmpty("participant"); !participant.IsEmpty() && participant != from {
		attrs["participant"] = participant
	}
	if recipient := ag.OptionalJIDOrEmpty("recipient"); !recipient.IsEmpty() {
		attrs["recipient"] = recipient
	}
	return waBinary.Node{Tag: "ack", Attrs: attrs}, true
}

func OfferHasVideo(offer *waBinary.Node) bool {
	if offer == nil {
		return false
	}
	for _, c := range wanode.NodeChildren(offer) {
		if c.Tag == "video" {
			return true
		}
	}
	return false
}

func ParseVideoState(inner *waBinary.Node) (state, orientation int) {
	state, _ = strconv.Atoi(wanode.AttrString(inner.Attrs, "state"))
	orientation, _ = strconv.Atoi(wanode.AttrString(inner.Attrs, "device_orientation"))
	return state, orientation
}

func videoOfferNode() waBinary.Node {
	return waBinary.Node{Tag: "video", Attrs: waBinary.Attrs{
		"enc":                "h.264",
		"dec":                "H264",
		"screen_width":       "1920",
		"screen_height":      "1080",
		"device_orientation": "0",
	}}
}

func videoAcceptNode() waBinary.Node {
	return waBinary.Node{Tag: "video", Attrs: waBinary.Attrs{
		"enc": "h.264",
		"dec": "H264,H265,AV1",
	}}
}

func videoPreacceptNode() waBinary.Node {
	return waBinary.Node{Tag: "video", Attrs: waBinary.Attrs{
		"dec":                "H264",
		"device_orientation": "0",
		"screen_width":       "0",
		"screen_height":      "0",
	}}
}
