# Video call signaling

**Files:** [`internal/voip/signaling/video.go`](../internal/voip/signaling/video.go),
[`internal/voip/signaling/signaling_build.go`](../internal/voip/signaling/signaling_build.go),
[`internal/voip/call/callmanager_video.go`](../internal/voip/call/callmanager_video.go),
[`internal/wa/socket.go`](../internal/wa/socket.go),
[`internal/app/session/session.go`](../internal/app/session/session.go)

WaCalls speaks the WhatsApp 1:1 video signaling protocol: it advertises video in the
offer/accept, drives mid-call upgrade and downgrade transitions, and answers the peer's
transitions with the typed ack WhatsApp requires.

**Scope note:** this doc is the signaling layer. The inbound media legs (H.264 receive +
browser display) landed and are documented in [video-media.md](./video-media.md), including
the WhatsApp downlink-bitrate limitation. WaCalls still sends no video of its own (receive
only). The state machine, the wire protocol, and the API here are complete on their own.

## Overview

A video call is either offered as video from the start (a `<video>` child in the offer) or
upgraded mid-call from an existing audio call. Upgrades are a small handshake carried by
standalone `<call><video state=N .../></call>` stanzas; the peer's transitions arrive the
same way and must be answered with a typed `<ack ... type="video">`.

- Outbound video call: `POST /api/sessions/{sid}/calls {"phone":"...","video":true}`.
- Drive the flow on a live call: `POST /api/sessions/{sid}/calls/{id}/video`
  `{"action":"request|accept|reject|stop|orientation","orientation":0-3}`.
- Observe the flow: the SSE `call-video` event `{local, remote, pending, orientation}`,
  where `pending` is `"out"` while our request awaits the peer, `"in"` while the peer's
  request awaits us, and `""` once settled.

## State enum

Standalone `<video>` transitions carry a numeric `state`:

| state | name | meaning |
|---|---|---|
| 0 | Disabled | camera off |
| 1 | Enabled | camera on / active |
| 3 | UpgradeRequest | legacy upgrade request (inbound only) |
| 4 | UpgradeAccept | accept an upgrade (`dec="H264,AV1"`) |
| 5 | UpgradeReject | decline an upgrade |
| 6 | Stopped | stop sending video |
| 8 | UpgradeCancel | requester cancels |
| 11 | UpgradeRequestV2 | the request modern clients send (`dec="H264"`, `voip_settings="video"`) |

Outbound, WaCalls sends only 11 (request), 4-then-1 (accept), 6 (stop), and 1 with a
`device_orientation` (orientation). Cancelling a pending request is `stop` while still
gated. States 3/5/8 are handled inbound.

## Upgrade flow

```
WaCalls                                    Peer
  | RequestVideoUpgrade                     |
  |--- <video state=11 dec=H264> ---------->|   (localVideo=true, gate=true: no media yet)
  |<-- <ack type="video"> -------- (peer)   |
  |<-- <video state=4> (accept) ------------|   gate clears, WaCalls announces:
  |--- <video state=1> -------------------->|
  |         ...both sides now "enabled"...  |
```

The gate (`videoGate`) stays set from the request until the peer confirms (state 4 or 1),
which is where a future media pipeline will hold RTP until the peer is ready. When both
sides request at once (glare), WaCalls treats it as mutual accept: it clears the gate and
announces state 1 rather than firing the inbound-request path.

## Capability bytes

The offer's `<capability>` blob carries a media-type byte. Answering a video call with the
audio blob was the v1 bug that kept video from ever working.

| blob | bytes | used for |
|---|---|---|
| audio offer | `01 05 f7 09 e4 bb 13` | audio offer/accept |
| video offer | `01 05 f7 09 e4 fa 13` | video offer (byte 5: `fa` = video) |
| audio preaccept | `01 05 f7 09 e4 bb 07` | audio preaccept |

A video preaccept uses the audio **offer** blob (ending `13`), not the preaccept blob.

## Footguns

- **The typed ack is load-bearing (symptom: upgrade cancels after ~5s).** whatsmeow acks
  every inbound `<call>` node with a bare, typeless ack. A `<video>` transition needs
  `<ack class="call" type="video">` echoing the sender's `participant`/`recipient` routing;
  without it the peer treats the upgrade as un-acked and cancels after about 5 seconds.
  WaCalls installs a raw `<call>` interceptor (reflection over whatsmeow's unexported
  `nodeHandlers`) so it claims video nodes and sends the typed ack itself. If a whatsmeow
  bump removes that seam, the interceptor fails to install and WaCalls degrades to
  dual-ack (whatsmeow's generic ack plus our typed one via the `UnknownCallEvent` path);
  `wacalls -doctor` reports this on the `video ack interceptor` line.

- **Capability byte drift.** The video blob starts from WaCalls' proven `0xe4` base
  (`01 05 f7 09 e4 fa 13`). If a video offer never rings as video on the official client,
  the first knob to try is byte 4 (`0xe4` -> `0xe0`), then compare the ack bytes.

- **In-call video stanzas route to the answering device.** Like mute and terminate, video
  transitions target `acceptedByJid` when a companion device answered, and inbound video
  stanzas from any other device are ignored (else a stale sibling device could drive the
  flow).

## Current behavior

- Inbound video call: rings with `video:true` in the SSE. It is answered advertising video
  (the preaccept carries the video capability `01 05 ff 09 e0 fa 13` and the accept a
  `<video enc="h.264" dec="H264,H265,AV1">` child) - the WhatsApp relay only bridges the video
  call's downlink once the callee advertises video, so answering audio-only left the receive
  path silent (field-confirmed 2026-07-17: `rtt_samples` 0 → 13 after advertising video).
  Audio is two-way and the peer's video **renders** in the browser (see
  [video-media.md](./video-media.md)), subject to the downlink-bitrate limitation documented
  there. WaCalls sends no video of its own, so the peer sees no picture from us.
- Outbound video call: fully signaled; the peer connects with two-way audio and no picture.
- Inbound upgrade request on an audio call: acked, surfaced on SSE, then auto-rejected by the
  session layer so the peer does not wait on a black tile (the media pipeline attaches on a
  video-from-start answer, not yet on a mid-call upgrade).
