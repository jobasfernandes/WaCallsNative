package appdata

import (
	"errors"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
)

var errMalformedPayload = errors.New("appdata: malformed reaction payload")

type reaction struct {
	transactionID uint64
	emoji         string
}

func encodeReaction(transactionID uint64, emoji string) []byte {
	r := protowire.AppendTag(nil, 1, protowire.VarintType)
	r = protowire.AppendVarint(r, transactionID)
	r = protowire.AppendTag(r, 2, protowire.BytesType)
	r = protowire.AppendString(r, emoji)

	msg := protowire.AppendTag(nil, 1, protowire.BytesType)
	msg = protowire.AppendBytes(msg, r)

	out := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(out, msg)
}

func decodeReactions(payload []byte) ([]reaction, error) {
	var out []reaction
	for len(payload) > 0 {
		num, typ, n := protowire.ConsumeTag(payload)
		if n < 0 {
			return nil, errMalformedPayload
		}
		payload = payload[n:]
		if num != 1 || typ != protowire.BytesType {
			skip := protowire.ConsumeFieldValue(num, typ, payload)
			if skip < 0 {
				return nil, errMalformedPayload
			}
			payload = payload[skip:]
			continue
		}
		msg, n := protowire.ConsumeBytes(payload)
		if n < 0 {
			return nil, errMalformedPayload
		}
		payload = payload[n:]
		r, err := decodeMessage(msg)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, errMalformedPayload
	}
	return out, nil
}

func decodeMessage(msg []byte) (reaction, error) {
	for len(msg) > 0 {
		num, typ, n := protowire.ConsumeTag(msg)
		if n < 0 {
			return reaction{}, errMalformedPayload
		}
		msg = msg[n:]
		if num != 1 || typ != protowire.BytesType {
			skip := protowire.ConsumeFieldValue(num, typ, msg)
			if skip < 0 {
				return reaction{}, errMalformedPayload
			}
			msg = msg[skip:]
			continue
		}
		body, n := protowire.ConsumeBytes(msg)
		if n < 0 {
			return reaction{}, errMalformedPayload
		}
		return decodeReactionBody(body)
	}
	return reaction{}, errMalformedPayload
}

func decodeReactionBody(body []byte) (reaction, error) {
	var r reaction
	for len(body) > 0 {
		num, typ, n := protowire.ConsumeTag(body)
		if n < 0 {
			return reaction{}, errMalformedPayload
		}
		body = body[n:]
		switch {
		case num == 1 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(body)
			if n < 0 {
				return reaction{}, errMalformedPayload
			}
			r.transactionID = v
			body = body[n:]
		case num == 2 && typ == protowire.BytesType:
			s, n := protowire.ConsumeString(body)
			if n < 0 {
				return reaction{}, errMalformedPayload
			}
			r.emoji = s
			body = body[n:]
		default:
			skip := protowire.ConsumeFieldValue(num, typ, body)
			if skip < 0 {
				return reaction{}, errMalformedPayload
			}
			body = body[skip:]
		}
	}
	// transactionID 0 nunca e emitido pelo remetente, entao serve como sentinela
	// de payload truncado ou de campo ausente.
	if r.transactionID == 0 || !utf8.ValidString(r.emoji) {
		return reaction{}, errMalformedPayload
	}
	return r, nil
}
