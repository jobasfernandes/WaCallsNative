package signaling

import (
	waBinary "go.mau.fi/whatsmeow/binary"
)

// BuildCallControlAck builds the typed ack a call-control action requires. The
// "type" attribute is load-bearing: a generic ack without it makes the peer
// revert the state it just announced.
func BuildCallControlAck(original *waBinary.Node, childTag string) (waBinary.Node, bool) {
	if original == nil || childTag == "" {
		return waBinary.Node{}, false
	}
	attrs := original.AttrGetter()
	id := attrs.String("id")
	from := attrs.JID("from")
	if id == "" || from.IsEmpty() {
		return waBinary.Node{}, false
	}
	ackAttrs := waBinary.Attrs{"class": "call", "id": id, "to": from, "type": childTag}
	if participant := attrs.OptionalJIDOrEmpty("participant"); !participant.IsEmpty() && participant != from {
		ackAttrs["participant"] = participant
	}
	if recipient := attrs.OptionalJIDOrEmpty("recipient"); !recipient.IsEmpty() {
		ackAttrs["recipient"] = recipient
	}
	return waBinary.Node{Tag: "ack", Attrs: ackAttrs}, true
}
