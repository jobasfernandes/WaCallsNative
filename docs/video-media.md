# Video media

**Files:** [`internal/voip/media/h264/depacketizer.go`](../internal/voip/media/h264/depacketizer.go),
[`internal/voip/call/callmanager_videomedia.go`](../internal/voip/call/callmanager_videomedia.go),
[`internal/voip/call/callmanager_media.go`](../internal/voip/call/callmanager_media.go),
[`internal/voip/transport/subscriptions.go`](../internal/voip/transport/subscriptions.go),
[`internal/app/webrtc.go`](../internal/app/webrtc.go),
[`internal/app/session/bridge.go`](../internal/app/session/bridge.go),
[`client/src/lib/webrtc.ts`](../client/src/lib/webrtc.ts),
[`client/src/lib/call/video-codec.ts`](../client/src/lib/call/video-codec.ts),
[`client/src/components/domain/call/CallCard.tsx`](../client/src/components/domain/call/CallCard.tsx)

WaCalls does two-way H.264 video on a 1:1 call: it receives the peer's video and renders it
in the browser call card, and it sends the operator's camera to the peer. Signaling that gets
the call to ring and connect as video lives in [video-signaling.md](./video-signaling.md).

> **Known limitation:** the peer's video arrives at a low bitrate (~25 kbps / ~5 fps) that
> WhatsApp's server-side estimator will not raise for WaCalls. The picture is correct but
> soft. See [The downlink-bitrate wall](#the-downlink-bitrate-wall) - do not re-try the
> levers already ruled out there.

## Inbound pipeline (peer -> browser)

```
relay ─(SRTP, PT 97)─> handleVideoPacket ─> videoAssembler ─> bridge.WriteVideo
                          |                    (RFC 6184           |
                          | CVO ext (clear)     depacketize,       | pion TrackLocalStaticSample{H264}
                          v                     Annex-B assemble)  v
                    OnPeerVideoRotation ───────────────────>  browser <video> (sendrecv transceiver)
```

- **SSRC lock.** The peer sends audio on SSRC counter 0 and video on counter 2. The first
  inbound non-audio RTP packet whose decrypted payload looks like an H.264 NAL header locks
  the video SSRC; later packets on other SSRCs are ignored. Video uses a dedicated SRTP
  context so its ROC never collides with audio.
- **Subscription.** On locking, WaCalls subscribes the peer video SSRC with the relay
  (`SetPeerVideoSsrc` + `ResendSubscriptions`) so the relay forwards the video leg. Without
  the subscription the relay bridges audio only.
- **Depacketize / assemble.** `depacketizer.go` implements RFC 6184 (single-NAL and FU-A);
  `videoAssembler` concatenates NALs into an Annex-B access unit and emits it on the RTP
  marker bit, with a duration from the 90 kHz timestamp delta.
- **Orientation.** The WhatsApp CVO rotation rides an unencrypted one-byte RTP header
  extension (profile `0xBEDE`); `parseCVORotation` reads it and the browser applies a CSS
  rotate so the picture is upright.
- **Browser leg.** The server writes access units to a pion `TrackLocalStaticSample{H264}`
  added to the browser peer connection; on a video call the client makes the H.264 transceiver
  `sendrecv` and binds the received track to a `<video>` element in the call card.

## Outbound pipeline (browser -> peer)

```
browser camera ─encode H264/RFC6184─> pc video track (sendrecv, 320x240@15fps, 180 kbps)
                                            │
                        pc.OnTrack ── ReadRTP loop ──> bridge.OnBrowserVideo(payload, ts, marker)
                                            │                       │
                        pump: 1s PLI + inbound-PLI forward          v
                                            │            CallManager.SendPeerVideo
                                            ▼               (rewrite header, SRTP, Broadcast)
                                  pc.WriteRTCP(PLI) to browser  ──> relay ─(SRTP, PT 97)─> peer
```

The server does **not** encode or re-packetize. The browser encodes and RFC-6184 packetizes
the camera; the server reuses those H.264 payloads verbatim and only rewrites the RTP header
and re-encrypts:

- **No server-side encode.** `pc.OnTrack` reads the browser's video RTP; each payload,
  timestamp (90 kHz, passed through), and marker goes to `SendPeerVideo`.
- **Header rewrite.** PT 97 (or the locked downlink PT), our own video SSRC
  `GenerateSecureSsrc(callID, ourDeviceJid, counter=2)` (audio is counter 0), our own
  sequence counter. Timestamp and marker come from the browser. **No RTP header extension on
  outbound video** - two-way video works without one; only outbound audio carries the empty
  `0xbede`.
- **SRTP.** A dedicated send context (its own ROC) keyed off the same per-JID `sendKM` as
  audio; the IV mixes in the SSRC so one key across both streams is safe. Auth tag 4 bytes.
- **Lazy declare.** The first camera frame derives the video SSRC, marks it self (so the
  inbound NAL-sniff never locks onto our own echo), and declares it to the relay
  (`SetVideoSsrc` + `ResendSubscriptions`).
- **Keyframes.** The WhatsApp peer sends no PLI, so the bridge pumps a PLI to the browser
  every 1 s once its video arrives; an inbound peer PLI/FIR (RTCP PSFB fmt 1/4) is also
  forwarded to the browser. Both make the browser encoder emit an IDR.
- **Encode caps.** The browser caps the stream at 320x240 / 15 fps / 180 kbps: a light stream
  freezes less on WhatsApp's loss-sensitive relay. No sender report is emitted for the video
  SSRC; two-way video works without one.

## The downlink-bitrate wall

**Symptom.** Inbound video opens with a brief burst (~85 kbps, ~13 fps) then settles at a
floor of ~25 kbps / ~5 fps for the rest of the call. `keyframe_waits` stays 0, so this is
not frame loss or a depacketizer stall - the peer is simply encoding at a low bitrate.

**It is not a hard cap on non-official clients.** A reference caller running WhatsApp's real
`whatsapp.wasm` VoIP engine, captured on the same 1:1 video call type, receives ~1 Mbps on a
single video SSRC (no simulcast). So the ceiling is reproducible-around: WhatsApp's sender
ramps its video encoder up for that client and not for WaCalls.

**Cause.** WhatsApp drives the sender's video bitrate with a proprietary bandwidth
estimator (telemetry name `one_side_bwe`, ML-assisted) that lives inside `whatsapp.wasm`.
It does **not** react to standard WebRTC feedback. The feedback it does consume (the relay
subscription payload, its RTCP application feedback, its bandwidth-report packets) is built
inside the WASM and carried SRTCP-encrypted / in encrypted STUN attributes, so it cannot be
read from a packet capture without the peer's keys, and cannot be reproduced from the
observable wire alone.

**Ruled out (measured, no effect on the floor) - do not re-try these blind:**

| Lever | Result |
|---|---|
| Relay subscription at stream layer 0 vs 1 | no change (audio-layer-0 proves plaintext subs are honored; video layer value is moot) |
| PRST / PSFB fmt=15 application feedback advertising 1 Mbit | no change |
| RTCP Receiver Reports for the video SSRC | no change |
| Compact RTCP 208/209 (send, and responding to inbound) | never received inbound in a video call - 208/209 is the **audio**-call mechanism |
| `device_class` on offer/accept | no change |
| Advertised `screen_width`/`screen_height` on the accept video node | no change |
| Video capability byte on the preaccept | no change |

**What would actually move it** is reproducing the WASM's estimator inputs - the exact
subscription payload, the bandwidth-report/AFB feedback format, and any RTP header extension
that seeds the estimate. Recovering those needs WASM-level reverse engineering (extracting
the reference's SRTP keys to decrypt its feedback, or disassembling the estimator). That is
a separate effort, not a signaling or RTCP tweak.

## Footguns

- **Do not read the floor as a pipeline bug.** `keyframe_waits=0` and steady `idr` counts in
  the `video rx summary` log mean WaCalls is receiving and assembling everything the peer
  sends. A low `kbps` there is the peer's encode bitrate, not dropped frames.
- **208/209 are audio-only.** A live video-call capture shows zero PT 208/209 in either
  direction; they are the audio call's `one_side_bwe` keepalive. Do not wire video bitrate
  logic to them.
- **The CVO extension is on the clear RTP header, not the SRTP payload.** Read it before
  decrypting; a mid-call orientation change arrives as a new extension value on the next
  packet.
