import { apiPost } from "./api";
import { setupAudioChannel } from "./call/audio-channel";
import { h264ReceiveCodecs } from "./call/video-codec";

export type OpenCall = {
  pc: RTCPeerConnection;
  micStream: MediaStream;
  remoteStream: MediaStream | null;
  remoteVideoStream: MediaStream;
  onRotation: (cb: (deg: number) => void) => void;
  close: () => void;
};

export const acquireMic = (micDeviceId: string | null): Promise<MediaStream> =>
  navigator.mediaDevices.getUserMedia({
    audio: micDeviceId ? { deviceId: { exact: micDeviceId } } : true,
    video: false,
  });

export const openCall = async (
  sid: string,
  callId: string,
  micStream: MediaStream,
): Promise<OpenCall> => {
  const localStream = micStream;

  const pc = new RTCPeerConnection({ iceServers: [] });
  const audio = await setupAudioChannel(pc, localStream);

  // Downlink-only video: receive the peer's H.264 stream. Camera upload is a later slice.
  const remoteVideoStream = new MediaStream();
  const videoTx = pc.addTransceiver("video", { direction: "recvonly" });
  try {
    if ("setCodecPreferences" in videoTx) {
      const h264 = h264ReceiveCodecs(RTCRtpReceiver.getCapabilities("video"));
      if (h264.length) videoTx.setCodecPreferences(h264);
    }
  } catch {}
  pc.addEventListener("track", (e) => {
    if (e.track.kind === "video") remoteVideoStream.addTrack(e.track);
  });

  // The server pushes the peer's video orientation over the "meta" control channel.
  let lastRotation = 0;
  let rotationCb: ((deg: number) => void) | null = null;
  const meta = pc.createDataChannel("meta", { ordered: true });
  meta.onmessage = (e) => {
    try {
      const rot = JSON.parse(e.data as string).rot;
      if (typeof rot === "number") {
        lastRotation = rot;
        rotationCb?.(rot);
      }
    } catch {}
  };

  const offer = await pc.createOffer();
  await pc.setLocalDescription(offer);
  await new Promise<void>((resolve) => {
    if (pc.iceGatheringState === "complete") resolve();
    else
      pc.addEventListener("icegatheringstatechange", () => {
        if (pc.iceGatheringState === "complete") resolve();
      });
  });

  const { sdp_answer } = await apiPost<{ sdp_answer: string }>(
    `/api/sessions/${sid}/calls/${callId}/webrtc`,
    { sdp_offer: pc.localDescription!.sdp },
  );
  await pc.setRemoteDescription({ type: "answer", sdp: sdp_answer });

  return {
    pc,
    micStream: localStream,
    remoteStream: audio.remoteStream,
    remoteVideoStream,
    onRotation: (cb) => {
      rotationCb = cb;
      cb(lastRotation);
    },
    close: () => {
      audio.close();
      try {
        localStream.getTracks().forEach((t) => t.stop());
      } catch {}
      try {
        pc.close();
      } catch {}
    },
  };
};
