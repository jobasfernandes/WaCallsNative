// Selects the browser's H.264 receive codecs so the video transceiver negotiates H.264
// (WhatsApp's codec) instead of the browser's default preference (often VP8). Pure and
// testable; returns the H.264 capability entries, or an empty list when unavailable.
export const h264ReceiveCodecs = (
  caps: RTCRtpCapabilities | null,
): RTCRtpCodec[] =>
  (caps?.codecs ?? []).filter((c) => c.mimeType.toLowerCase() === "video/h264");

// Maps the WhatsApp CVO orientation (degrees, already 0/90/180/270) to the CSS rotation that
// renders the peer's portrait video upright. The video is rotated by the negative of the
// reported orientation, matching the official client.
export const cvoRotationToCss = (deg: number): number =>
  -(((deg % 360) + 360) % 360);
