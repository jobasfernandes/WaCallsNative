# Group call signaling

**Files:** [`internal/voip/signaling/group_types.go`](../internal/voip/signaling/group_types.go)
· [`group_parse.go`](../internal/voip/signaling/group_parse.go)
· [`group_build.go`](../internal/voip/signaling/group_build.go)
· [`calllink.go`](../internal/voip/signaling/calllink.go)
· [`participant.go`](../internal/voip/signaling/participant.go)
· [`ack.go`](../internal/voip/signaling/ack.go)
· [`internal/voip/transport/subscriptions_group.go`](../internal/voip/transport/subscriptions_group.go)
· [`ssrc_group.go`](../internal/voip/transport/ssrc_group.go)
· [`forwarding.go`](../internal/voip/transport/forwarding.go)
· [`internal/voip/media/rtcp_group.go`](../internal/voip/media/rtcp_group.go)

## Overview

> **This is signaling only.** Nothing here sends or receives. A group call does
> not work with this layer alone: it builds and parses the stanzas and the relay
> byte payloads, and stops there. The media plane (participant roster, key
> epochs, per-participant SRTP, audio mixing) is a separate piece of work.

What it covers: starting a group call, inviting participants, the authoritative
roster snapshot, the shared key epoch, call links, the waiting room, raise hand,
and screen share.

## Reading the control plane

The whatsmeow client types only `offer`, `offer_notice`, `accept`, `preaccept`,
`transport`, `terminate`, `reject` and `relaylatency`. **Everything else arrives
as `*waevents.UnknownCallEvent` carrying the raw node** — which is the same path
`mute_v2` already uses in `internal/app/session/session.go`.

So the five group control actions (`group_update`, `enc_rekey`,
`waiting_room_update`, `user_action`, `screen_share`) are read with
`ParseCallControlEnvelope`, and **no reflection or `unsafe` is needed**. The
reference implementation patches whatsmeow's unexported handler map to do this;
we do not have to.

### Every control action needs a typed ack

`BuildCallControlAck(original, childTag)` produces it. The `type` attribute is
load-bearing: **a generic ack without it makes the peer revert the state it just
announced.** The ack also propagates `participant` and `recipient` when present,
without which a multi-device peer gets it on the wrong device.

## SSRC slots

Both sides derive the same SSRCs from `(callID, deviceJID, counter)`, which is
how they find each other's streams without SDP.

| Stream | Counter |
|---|---|
| Audio | 0 |
| Video | 2 |
| App-data (reactions) | 6 |

The nine relay streams the allocate advertises follow the slot plan
`{0, 1, 4, 2, 3, 5, 7, 8, 6}`, and hop-by-hop FEC uses slot words 7 and 8.

### Why the three auxiliary slots are random

Index 8 of that plan carries slot word 6 — **the same slot as app-data**.
Deriving all nine naively announces one SSRC in two subscription groups
(secondary video and app-data), which the captured client never does.

`PrepareRelayStreamSSRCs` therefore overwrites indices 6, 7 and 8 with random
values, excluding the app-data SSRC and the first six from the candidate set. The
random source is injected so the test is deterministic. There is a test that
asserts the naive derivation *does* collide, so if that ever stops being true the
function's reason for existing is re-examined rather than silently kept.

## The group allocate

Attribute order, matching the capture:

| Attribute | Contents |
|---|---|
| `0x4000` | relay token |
| `0x4025` | sender subscriptions |
| `0x4021` | receiver subscriptions |
| `0x4024` | stream descriptors |
| `0x805a` | participant count |
| `0x0016` | relay endpoint |
| `0x0008` | message integrity |

The four sender subscription groups go out in this order: primary video (flagged
as video), secondary video (no participants), audio, app-data.

**The hop-by-hop FEC descriptors are only sent with more than one remote
participant**, as participants 3 and 4 on layer 3.

## Capability bytes

This repo uses `{0x01, 0x05, 0xf7, 0x09, 0xe4, 0xbb, 0x13}` for the offer, which
is validated in the field on 1:1 calls. The reference implementation uses `0xe0`
in byte 5 instead, and neither documents what the byte means.

We keep ours, and it only applies to offers we originate: the capability of every
*other* device comes echoed from the `<group_info>` the server sent, and the
parser preserves it byte for byte because the accept has to send it back
unchanged.

**If the server rejects a group offer with 439, the capability byte is the first
suspect** and switching byte 5 to `0xe0` is the first experiment. The child order
of `<offer>` is the second: it is load-bearing, and there is a test pinning it.

## Waiting room

Entering the waiting room is derived **by absence**: the server answers a held
join with `<waiting_room>` and no `<group_info>`. With a roster present the
participant is admitted, even when a waiting room exists.

Leaving it is not observable from this layer: it happens when the first
authoritative `group_update` arrives, which belongs to the media/control plane.

Both the waiting room and the roster carry a `transaction-id` that orders
snapshots. This layer only parses it; discarding stale updates is the consumer's
job, and is deliberately not implemented twice.

## Known limitation, inherited

The reference implementation **never managed a group call with two remote
participants**. Two live attempts are recorded, both lost inbound media, and both
only recovered when the roster shrank back to one participant:

| Call | Symptom |
|---|---|
| one | video lost for ~12s after the two-PID allocate |
| two | **all** inbound RTP lost for ~53s |

The suspected cause is the missing hop-by-hop FEC descriptors, and the fix for it
was committed twelve minutes after the last failing call and **was never
retested live**. Its own datasheet still lists the causal claim under "inferences
to validate live", and names competing explanations that were not eliminated: the
capture sends the descriptors and the receiver subscriptions as *two* packets 59
ms apart, while a single packet is what gets sent here.

Practical reading for whoever implements the media plane: **treat
multi-participant as unproven**. One remote participant is the only configuration
with positive evidence. Budget time for capturing your own two-participant
allocate before assuming the bug is in your own code.

## Related

- [call-reactions.md](./call-reactions.md) for the app-data stream, which shares
  the SSRC slot map
- [troubleshooting.md](./troubleshooting.md) for audio and ICE problems
