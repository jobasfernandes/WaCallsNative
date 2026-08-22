# Call reactions

**Files:** [`internal/voip/extension/appdata/`](../internal/voip/extension/appdata/)
· [`internal/voip/core/types.go`](../internal/voip/core/types.go)
· [`internal/voip/core/ports.go`](../internal/voip/core/ports.go)
· [`internal/voip/engine/scope.go`](../internal/voip/engine/scope.go)
· [`internal/voip/call/callmanager.go`](../internal/voip/call/callmanager.go)
· [`internal/app/handlers_call.go`](../internal/app/handlers_call.go)
· [`client/src/components/domain/call/CallCard.tsx`](../client/src/components/domain/call/CallCard.tsx)

## Overview

During a 1:1 call either side can send an emoji reaction. WhatsApp carries it as
**media, not signaling**: the reaction rides a dedicated RTC "app-data" RTP
stream alongside the audio, encrypted with the same SRTP keys.

That choice has three consequences worth knowing before touching this code:

- A reaction only works while media is up. There is no `<call>` stanza for it, so
  a call that is ringing but not yet connected cannot carry one.
- It is **transient**. Reactions are never written to `CallRecord`, never
  persisted, and never sent to webhooks. They reach the browser as an SSE event
  and nothing else, like `call-peer-mute` and `call-quality`.
- It costs relay bandwidth. Each reaction is sent **ten times** (see
  [Retransmission](#retransmission-and-dedup)), which is why the send path is
  rate limited.

## Wire format

| Property | Value |
|---|---|
| RTP payload type | `119` (`core.PayloadTypeWhatsAppAppData`) |
| SSRC counter | `6` (`core.SsrcCounterAppData`) |
| RTP header size | 16 bytes |
| Extension profile | `0xdebe`, with **zero** extension words |
| Marker bit | always `0` |
| Encryption | same per-JID SRTP key as audio, plus the 4-byte WARP MI tag |

SSRC counters index the ~12-SSRC set WhatsApp allocates per device. Both sides
derive the same values, which is how they learn each other's SSRCs without SDP:

| Stream | Counter |
|---|---|
| Audio | `0` |
| Video | `2` |
| App-data (reactions) | `6` |

```
ssrc = LE_uint32( HKDF-SHA256(secret=callID, salt=counter_LE32, info=deviceJID, 4) )
```

The header layout of the first packet of a reaction:

```
off 0 : 90            V=2 P=0 X=1 CC=0
off 1 : 77            M=0, PT=119
off 2 : seq BE        starts at 1
off 4 : timestamp BE  starts at 50, step 50
off 8 : SSRC BE       app-data SSRC, counter 6
off 12: de be         extension profile
off 14: 00 00         extension length = 0 words
```

The timestamp is **synthetic**. It tracks no media clock: not the 16 kHz audio
rate, not the 90 kHz video rate. Do not try to align it with either.

### Payload

Three nested protobuf messages, built with `protowire` (no `.proto`, no generated
code):

```
Payloads { repeated Message payloads = 1; }
Message  { Reaction reaction = 1; }
Reaction { uint64 transaction_id = 1; string emoji = 2; }
```

A thumbs-up with `transaction_id = 1` is exactly twelve bytes:

```
0a 0a  0a 08  08 01  12 04 f0 9f 91 8d
└payloads     └txid=1 └emoji (UTF-8)
      └message
```

An **empty emoji clears** the previous reaction. The protocol has no emoji
allowlist; any valid UTF-8 is accepted on the wire, and validation happens only
at the HTTP boundary (`validReactionEmoji`).

## Flow

Outbound:

```
POST /api/sessions/{sid}/calls/{id}/reaction  {"emoji":"..."}
  -> Session.SendReaction
  -> call.Client.SendReaction -> CallManager.SendReaction   (rate limit here)
  -> engine.Capability[core.ReactionSink]
  -> appdata: encode protobuf, build RTP header (PT 119, app-data SSRC)
  -> CallScope.SendRTP -> SrtpManager.Protect -> Relay.Broadcast
```

Inbound:

```
relay -> CallManager.handleRTP  (dispatch by payload type)
  -> rtpHandlers[119]  (registered by the extension via CallScope.OnRTP)
  -> SrtpManager.Unprotect -> decode protobuf -> dedup
  -> CallManager.OnReaction -> Broker.EmitCallReaction
  -> SSE "call-reaction" -> zustand store -> CallCard
```

## The relay never learns the SSRC from signaling

In a 1:1 call the app-data SSRC is **not advertised** anywhere: not in the STUN
allocate, not in the subscriptions, not in the keepalive refresh. The relay
learns it from the first RTP packet that arrives on it.

This is why adding reactions required no change to `transport/`. If you ever see
reactions failing to reach the peer, the allocate is not the place to look.

## Retransmission and dedup

The sender emits **ten identical copies** of each reaction, one every 50 ms, each
with a fresh sequence number and the timestamp advanced by 50. The payload bytes
are identical across all ten.

The receiver keeps a **high-water mark** of the peer's `transaction_id`: the first
copy that authenticates is delivered, the other nine are dropped silently. The
same rule discards ids that arrive out of order.

The trade: losing a packet costs nothing, because any one of the ten carries the
whole reaction. Losing the first few just delays the reaction by up to 450 ms.

> **Footgun.** The high-water mark **must be reset when the media session
> restarts**, because the peer's sender starts counting from one again. A stale
> mark silently swallows every later reaction, with no error and no log. The reset
> is wired into both `cleanupMedia` and `reinitSrtpLocked`
> (`CallManager.resetReactionState`); if you add a third path that re-establishes
> media, it needs the reset too.

The first copy is sent **synchronously** so an immediate transport failure still
reaches the HTTP caller; the remaining nine go out on a tracked goroutine, so the
call's leak accounting stays correct.

## Rate limit

One reaction per call every **500 ms** (`reactionMinInterval`), enforced in
`CallManager`, not in the HTTP handler, so it also covers callers that do not come
through the API. Excess returns a `*call.CallError`, which the handler maps to
HTTP 409.

Ten packets per reaction is the reason: without a limit, a client repeating the
click saturates the relay uplink and competes with audio.

## Quality metrics

Only the audio stream feeds the RTCP receiver statistics. App-data packets are
sporadic and carry their own sequence space, so counting them as audio reads as
catastrophic loss: a two-packet app-data burst with a sequence gap produced a
**99.98% false loss reading** before the payload-type gate was added in
`handleRTP`. Any new media stream must stay out of `recvStats.NoteRTP` too.

## Related

- [deploy.md](./deploy.md) for standing up the server
- [troubleshooting.md](./troubleshooting.md) for audio and ICE problems
