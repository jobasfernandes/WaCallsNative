import { describe, it, expect } from "vitest";
import { h264ReceiveCodecs, cvoRotationToCss } from "./video-codec";

describe("h264ReceiveCodecs", () => {
  it("keeps only H.264 codecs, case-insensitive", () => {
    const caps = {
      codecs: [
        { mimeType: "video/VP8" },
        { mimeType: "video/H264", sdpFmtpLine: "a" },
        { mimeType: "video/h264", sdpFmtpLine: "b" },
        { mimeType: "video/rtx" },
      ],
      headerExtensions: [],
    } as unknown as RTCRtpCapabilities;
    const got = h264ReceiveCodecs(caps);
    expect(got.map((c) => c.mimeType)).toEqual(["video/H264", "video/h264"]);
  });

  it("returns empty when capabilities are null", () => {
    expect(h264ReceiveCodecs(null)).toEqual([]);
  });
});

describe("cvoRotationToCss", () => {
  it("negates the reported orientation to render upright", () => {
    expect(cvoRotationToCss(0)).toBe(-0);
    expect(cvoRotationToCss(90)).toBe(-90);
    expect(cvoRotationToCss(270)).toBe(-270);
  });

  it("normalizes out-of-range degrees", () => {
    expect(cvoRotationToCss(360)).toBe(-0);
    expect(cvoRotationToCss(-90)).toBe(-270);
  });
});
