import { apiPost } from "./api";
import { setupAudioChannel } from "./call/audio-channel";
import { h264ReceiveCodecs } from "./call/video-codec";

export type OpenCall = {
  pc: RTCPeerConnection;
  micStream: MediaStream;
  remoteStream: MediaStream | null;
  remoteVideoStream: MediaStream;
  localVideoStream: MediaStream | null;
  toggleCamera: () => boolean;
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
  video = false,
): Promise<OpenCall> => {
  const localStream = micStream;

  const pc = new RTCPeerConnection({ iceServers: [] });
  const audio = await setupAudioChannel(pc, localStream);

  // Video: receive the peer's H.264 stream and, on a video call, also send our camera.
  const remoteVideoStream = new MediaStream();
  let localVideoStream: MediaStream | null = null;
  if (video) {
    try {
      localVideoStream = await navigator.mediaDevices.getUserMedia({
        video: {
          width: { ideal: 320 },
          height: { ideal: 240 },
          frameRate: { ideal: 15, max: 15 },
        },
      });
    } catch {
      localVideoStream = null;
    }
  }
  const videoTx = pc.addTransceiver("video", {
    direction: localVideoStream ? "sendrecv" : "recvonly",
  });
  try {
    if ("setCodecPreferences" in videoTx) {
      const h264 = h264ReceiveCodecs(RTCRtpReceiver.getCapabilities("video"));
      if (h264.length) videoTx.setCodecPreferences(h264);
    }
  } catch {}
  if (localVideoStream) {
    // A light stream (320x240, 180 kbps, 15 fps) freezes less on WhatsApp's loss-sensitive relay.
    await videoTx.sender.replaceTrack(localVideoStream.getVideoTracks()[0]);
    try {
      const params = videoTx.sender.getParameters();
      if (!params.encodings || params.encodings.length === 0) {
        params.encodings = [{}];
      }
      params.encodings[0].maxBitrate = 180_000;
      params.encodings[0].maxFramerate = 15;
      await videoTx.sender.setParameters(params);
    } catch {}
  }
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

  try {
    const { sdp_answer } = await apiPost<{ sdp_answer: string }>(
      `/api/sessions/${sid}/calls/${callId}/webrtc`,
      { sdp_offer: pc.localDescription!.sdp },
    );
    await pc.setRemoteDescription({ type: "answer", sdp: sdp_answer });
  } catch (err) {
    // Release the camera we captured above; the caller's catch only stops the mic it owns.
    try {
      localVideoStream?.getTracks().forEach((t) => t.stop());
    } catch {}
    try {
      pc.close();
    } catch {}
    throw err;
  }

  return {
    pc,
    micStream: localStream,
    remoteStream: audio.remoteStream,
    remoteVideoStream,
    localVideoStream,
    toggleCamera: () => {
      const track = localVideoStream?.getVideoTracks()[0];
      if (!track) return false;
      track.enabled = !track.enabled;
      return track.enabled;
    },
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
        localVideoStream?.getTracks().forEach((t) => t.stop());
      } catch {}
      try {
        pc.close();
      } catch {}
    },
  };
};
