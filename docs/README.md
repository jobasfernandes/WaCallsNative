# WaCalls docs

Curated, versioned documentation. Task-routed: find the guide by what you are doing.

| Guide | When to use |
|---|---|
| [deploy.md](./deploy.md) | Standing up WaCalls: Docker Compose, Traefik + TLS, or Swarm; env vars; WebRTC networking; pairing |
| [troubleshooting.md](./troubleshooting.md) | Something is wrong: no audio, one-way audio, ICE fails, login/session behind a proxy, rate limiting, pairing |
| [call-reactions.md](./call-reactions.md) | Working on emoji reactions: the app-data RTP stream, its wire format, retransmission and dedup, and the reset footgun |
| [group-calls.md](./group-calls.md) | Group call signaling, call links and the waiting room: stanza shapes, SSRC slots, the group allocate, and the unproven multi-participant path |

The project [README](../README.md) covers architecture, the call flow, the API,
and local development.
