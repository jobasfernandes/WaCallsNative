package signaling

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"

	waBinary "go.mau.fi/whatsmeow/binary"
)

// maxIndexedTokens bounds the token table so a hostile id attribute cannot make
// the parser allocate an arbitrarily long slice.
const maxIndexedTokens = 64

// ParseCallControlEnvelope extracts the single action from a raw call node.
func ParseCallControlEnvelope(node *waBinary.Node) (*CallControlEnvelope, error) {
	if node == nil || node.Tag != "call" {
		return nil, fmt.Errorf("signaling: parse call control envelope: unexpected node")
	}
	children := node.GetChildren()
	if len(children) != 1 {
		return nil, fmt.Errorf("signaling: parse call control envelope: got %d actions, want 1", len(children))
	}
	attrs := node.AttrGetter()
	childAttrs := children[0].AttrGetter()
	envelope := &CallControlEnvelope{
		From:        attrs.JID("from"),
		Participant: attrs.OptionalJIDOrEmpty("participant"),
		Recipient:   attrs.OptionalJIDOrEmpty("recipient"),
		Timestamp:   attrs.OptionalUnixTime("t"),
		CallID:      childAttrs.String("call-id"),
		CallCreator: childAttrs.JID("call-creator"),
		Action:      children[0],
	}
	if err := attrs.Error(); err != nil {
		return nil, fmt.Errorf("signaling: parse call control routing: %w", err)
	}
	if err := childAttrs.Error(); err != nil {
		return nil, fmt.Errorf("signaling: parse call control identity: %w", err)
	}
	return envelope, nil
}

// ParseGroupInviteSnapshot reads the roster an invite offer carries. ok is false
// when the offer is not a group invite, which is not an error.
func ParseGroupInviteSnapshot(offer *waBinary.Node) (*GroupCallUpdate, bool, error) {
	if offer == nil || offer.Tag != "offer" {
		return nil, false, fmt.Errorf("signaling: parse group invite snapshot: unexpected offer")
	}
	groupInfo, ok := offer.GetOptionalChildByTag("group_info")
	if !ok {
		return nil, false, nil
	}
	attrs := offer.AttrGetter()
	update := &GroupCallUpdate{CallID: attrs.String("call-id"), CallCreator: attrs.JID("call-creator")}
	if err := attrs.Error(); err != nil {
		return nil, false, fmt.Errorf("signaling: parse group invite identity: %w", err)
	}
	groupAttrs := groupInfo.AttrGetter()
	if callID := groupAttrs.OptionalString("call-id"); callID != "" && callID != update.CallID {
		return nil, false, fmt.Errorf("signaling: group invite call ID mismatch")
	}
	if creator := groupAttrs.OptionalJIDOrEmpty("call-creator"); !creator.IsEmpty() && creator != update.CallCreator {
		return nil, false, fmt.Errorf("signaling: group invite creator mismatch")
	}
	if err := parseGroupInfo(&groupInfo, update); err != nil {
		return nil, false, err
	}
	update.Joinable = attrs.OptionalString("joinable") == "1"
	return update, true, nil
}

// ParseInitialGroupCallAck parses the roster an ack carries, which is how both a
// group offer and a call-link join learn the authoritative snapshot.
func ParseInitialGroupCallAck(node *waBinary.Node) (*GroupCallUpdate, bool, error) {
	if node == nil || node.Tag != "ack" {
		return nil, false, fmt.Errorf("signaling: parse initial group ACK: unexpected envelope")
	}
	groupInfo, ok := node.GetOptionalChildByTag("group_info")
	if !ok {
		return nil, false, nil
	}
	attrs := groupInfo.AttrGetter()
	update := &GroupCallUpdate{CallID: attrs.String("call-id"), CallCreator: attrs.JID("call-creator")}
	if err := attrs.Error(); err != nil {
		return nil, true, fmt.Errorf("signaling: parse initial group identity: %w", err)
	}
	if err := parseGroupInfo(&groupInfo, update); err != nil {
		return nil, true, err
	}
	if relay, ok := node.GetOptionalChildByTag("relay"); ok {
		parsed, err := parseGroupRelay(&relay)
		if err != nil {
			return nil, true, err
		}
		update.Relay = parsed
	}
	return update, true, nil
}

// ParseGroupUpdate parses a group_update action.
func ParseGroupUpdate(node *waBinary.Node) (*GroupCallUpdate, error) {
	if node == nil || node.Tag != "group_update" {
		return nil, fmt.Errorf("signaling: parse group update: unexpected node")
	}
	attrs := node.AttrGetter()
	update := &GroupCallUpdate{CallID: attrs.String("call-id"), CallCreator: attrs.JID("call-creator")}
	if err := attrs.Error(); err != nil {
		return nil, fmt.Errorf("signaling: parse group update identity: %w", err)
	}
	groupInfo, ok := node.GetOptionalChildByTag("group_info")
	if !ok {
		return nil, fmt.Errorf("signaling: parse group update: missing group_info")
	}
	if err := parseGroupInfo(&groupInfo, update); err != nil {
		return nil, err
	}
	if avUpgrade, ok := node.GetOptionalChildByTag("av_upgrade"); ok {
		update.AVUpgradable = avUpgrade.AttrGetter().OptionalString("av-upgradable") == "1"
	}
	if relay, ok := node.GetOptionalChildByTag("relay"); ok {
		parsed, err := parseGroupRelay(&relay)
		if err != nil {
			return nil, err
		}
		update.Relay = parsed
	}
	return update, nil
}

// ParseGroupCallEncRekey parses one shared-key epoch delivered to this device.
// Only keygen 2 with a v2 Signal envelope is accepted: the group media plane
// cannot key off anything else.
func ParseGroupCallEncRekey(node *waBinary.Node) (*GroupCallEncRekey, error) {
	if node == nil || node.Tag != "enc_rekey" {
		return nil, fmt.Errorf("signaling: parse group rekey: unexpected node")
	}
	attrs := node.AttrGetter()
	rekey := &GroupCallEncRekey{CallID: attrs.String("call-id"), CallCreator: attrs.JID("call-creator")}
	var err error
	if rekey.TransactionID, err = requiredUint32Attr(attrs, "transaction-id"); err != nil {
		return nil, err
	}
	encopt, hasEncopt := node.GetOptionalChildByTag("encopt")
	enc, hasEnc := node.GetOptionalChildByTag("enc")
	if !hasEncopt || !hasEnc {
		return nil, fmt.Errorf("signaling: parse group rekey: missing encopt or enc")
	}
	if rekey.KeyGeneration, err = requiredUint32Attr(encopt.AttrGetter(), "keygen"); err != nil || rekey.KeyGeneration != 2 {
		return nil, fmt.Errorf("signaling: parse group rekey: unsupported key generation")
	}
	encAttrs := enc.AttrGetter()
	rekey.EncryptionType = encAttrs.String("type")
	if rekey.EncryptionVersion, err = requiredUint32Attr(encAttrs, "v"); err != nil {
		return nil, err
	}
	if (rekey.EncryptionType != "msg" && rekey.EncryptionType != "pkmsg") || rekey.EncryptionVersion != 2 {
		return nil, fmt.Errorf("signaling: parse group rekey: unsupported encryption")
	}
	ciphertext, ok := enc.Content.([]byte)
	if !ok {
		return nil, fmt.Errorf("signaling: parse group rekey: ciphertext is %T", enc.Content)
	}
	rekey.Ciphertext = bytes.Clone(ciphertext)
	return rekey, nil
}

func parseGroupInfo(node *waBinary.Node, update *GroupCallUpdate) error {
	attrs := node.AttrGetter()
	update.GroupJID = attrs.OptionalJIDOrEmpty("group-jid")
	update.Media = attrs.String("media")
	update.Joinable = attrs.OptionalString("joinable") == "1"
	update.RekeyRequested = attrs.OptionalString("rekey") == "1"
	var err error
	if update.TransactionID, err = requiredUint32Attr(attrs, "transaction-id"); err != nil {
		return fmt.Errorf("signaling: parse group transaction ID: %w", err)
	}
	if update.ConnectedLimit, err = requiredUint32Attr(attrs, "connected-limit"); err != nil {
		return fmt.Errorf("signaling: parse group connected limit: %w", err)
	}
	if err = attrs.Error(); err != nil {
		return fmt.Errorf("signaling: parse group_info attributes: %w", err)
	}
	for _, child := range node.GetChildren() {
		if child.Tag != "user" {
			continue
		}
		participant, parseErr := parseGroupParticipant(&child)
		if parseErr != nil {
			return parseErr
		}
		update.Participants = append(update.Participants, participant)
	}
	return nil
}

func parseGroupParticipant(node *waBinary.Node) (GroupCallParticipant, error) {
	attrs := node.AttrGetter()
	participant := GroupCallParticipant{
		JID: attrs.JID("jid"), PN: attrs.OptionalJIDOrEmpty("user_pn"),
		State: attrs.String("state"), Type: attrs.OptionalString("type"),
	}
	if err := attrs.Error(); err != nil {
		return participant, fmt.Errorf("signaling: parse group participant: %w", err)
	}
	for _, child := range node.GetChildren() {
		if child.Tag != "device" {
			continue
		}
		device, err := parseGroupDevice(&child)
		if err != nil {
			return participant, err
		}
		participant.Devices = append(participant.Devices, device)
	}
	return participant, nil
}

func parseGroupDevice(node *waBinary.Node) (GroupCallDevice, error) {
	attrs := node.AttrGetter()
	device := GroupCallDevice{JID: attrs.JID("jid"), Platform: attrs.OptionalString("platform")}
	var err error
	if device.PID, device.HasPID, err = optionalUint32Attr(attrs, "pid"); err != nil {
		return device, fmt.Errorf("signaling: parse group device PID: %w", err)
	}
	if err = attrs.Error(); err != nil {
		return device, fmt.Errorf("signaling: parse group device: %w", err)
	}
	if capability, ok := node.GetOptionalChildByTag("capability"); ok {
		capAttrs := capability.AttrGetter()
		device.CapabilityVersion, _, err = optionalUint32Attr(capAttrs, "ver")
		if err != nil {
			return device, fmt.Errorf("signaling: parse device capability: %w", err)
		}
		// Cloned because the accept echoes it back byte for byte, and the source
		// node is owned by the client that dispatched it.
		device.Capability = bytes.Clone(groupNodeBytes(&capability))
	}
	return device, nil
}

func parseGroupRelay(node *waBinary.Node) (*GroupCallRelay, error) {
	attrs := node.AttrGetter()
	relay := &GroupCallRelay{
		UUID: attrs.String("uuid"), ParticipantUUID: attrs.String("participant_uuid"),
		AttributePadding: attrs.OptionalString("attribute_padding") == "1",
		Tokens:           parseIndexedTokens(node, "token"), AuthTokens: parseIndexedTokens(node, "auth_token"),
	}
	var err error
	if relay.TransactionID, _, err = optionalUint32Attr(attrs, "transaction-id"); err != nil {
		return nil, err
	}
	if relay.SelfPID, relay.HasSelfPID, err = optionalUint32Attr(attrs, "self_pid"); err != nil {
		return nil, err
	}
	if relay.WarpMITagLength, relay.HasWarpMITagLength, err = optionalUint32Attr(attrs, "warp_mi_tag_len"); err != nil {
		return nil, err
	}
	if err = attrs.Error(); err != nil {
		return nil, fmt.Errorf("signaling: parse group relay: %w", err)
	}
	for _, child := range node.GetChildren() {
		switch child.Tag {
		case "key":
			relay.Key = bytes.Clone(groupNodeBytes(&child))
		case "hbh_key":
			relay.HBHKey = bytes.Clone(groupNodeBytes(&child))
		case "te2":
			endpoint, parseErr := parseGroupRelayEndpoint(&child)
			if parseErr != nil {
				return nil, parseErr
			}
			relay.Endpoints = append(relay.Endpoints, endpoint)
		}
	}
	return relay, nil
}

func parseGroupRelayEndpoint(node *waBinary.Node) (GroupCallRelayEndpoint, error) {
	attrs := node.AttrGetter()
	endpoint := GroupCallRelayEndpoint{
		RelayName: attrs.String("relay_name"), DomainName: attrs.OptionalString("domain_name"),
		IsFNA: attrs.OptionalString("is_fna") == "1", Address: bytes.Clone(groupNodeBytes(node)),
	}
	if len(endpoint.Address) == net.IPv4len+2 {
		endpoint.IPv4 = net.IP(endpoint.Address[:net.IPv4len]).String()
		endpoint.Port = binary.BigEndian.Uint16(endpoint.Address[net.IPv4len:])
	}
	var err error
	if endpoint.RelayID, _, err = optionalUint32Attr(attrs, "relay_id"); err != nil {
		return endpoint, err
	}
	if endpoint.TokenID, _, err = optionalUint32Attr(attrs, "token_id"); err != nil {
		return endpoint, err
	}
	if endpoint.AuthTokenID, _, err = optionalUint32Attr(attrs, "auth_token_id"); err != nil {
		return endpoint, err
	}
	if endpoint.RTT, _, err = optionalUint32Attr(attrs, "c2r_rtt"); err != nil {
		return endpoint, err
	}
	if err = attrs.Error(); err != nil {
		return endpoint, fmt.Errorf("signaling: parse group relay endpoint: %w", err)
	}
	return endpoint, nil
}

func optionalUint32Attr(attrs *waBinary.AttrUtility, key string) (uint32, bool, error) {
	raw, ok := attrs.GetString(key, false)
	if !ok {
		return 0, false, nil
	}
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, true, fmt.Errorf("invalid %s %q: %w", key, raw, err)
	}
	return uint32(value), true, nil
}

func requiredUint32Attr(attrs *waBinary.AttrUtility, key string) (uint32, error) {
	value, ok, err := optionalUint32Attr(attrs, key)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("missing %s", key)
	}
	return value, nil
}

func groupNodeBytes(node *waBinary.Node) []byte {
	switch content := node.Content.(type) {
	case []byte:
		return content
	case string:
		return []byte(content)
	default:
		return nil
	}
}

// parseIndexedTokens reads a token table addressed by an id attribute rather than
// by position: the relay references tokens by index from its endpoints.
func parseIndexedTokens(node *waBinary.Node, tag string) [][]byte {
	var tokens [][]byte
	for _, child := range node.GetChildren() {
		if child.Tag != tag {
			continue
		}
		value := groupNodeBytes(&child)
		if value == nil {
			continue
		}
		index := len(tokens)
		if raw := child.AttrGetter().OptionalString("id"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 || parsed >= maxIndexedTokens {
				continue
			}
			index = parsed
		}
		for len(tokens) <= index {
			tokens = append(tokens, nil)
		}
		tokens[index] = bytes.Clone(value)
	}
	return tokens
}
