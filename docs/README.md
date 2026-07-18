# WaCalls docs

Curated, versioned documentation. Task-routed: find the guide by what you are doing.

| Guide | When to use |
|---|---|
| [deploy.md](./deploy.md) | Standing up WaCalls: Docker Compose, Traefik + TLS, or Swarm; env vars; WebRTC networking; pairing |
| [troubleshooting.md](./troubleshooting.md) | Something is wrong: no audio, one-way audio, ICE fails, login/session behind a proxy, rate limiting, pairing |
| [video-signaling.md](./video-signaling.md) | Working on video calls: the offer/accept advertisement, upgrade state machine, capability bytes, the typed-ack requirement, the `/video` endpoint and `call-video` event |
| [video-media.md](./video-media.md) | Inbound video media: the H.264 receive pipeline, browser display, CVO orientation, and the downlink-bitrate limitation (levers already ruled out) |

The project [README](../README.md) covers architecture, the call flow, the API,
and local development.
