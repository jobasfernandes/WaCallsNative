import { apiPost } from "./api";
import { float32ToInt16LE, int16LEToFloat32 } from "./pcm";
import { VideoReceiver, VideoSender, videoSupported } from "./video-codec";
import {
  CAPTURE_PROCESSOR_NAME,
  CAPTURE_WORKLET_URL,
  PCM_CHANNEL_LABEL,
  PLAYBACK_PROCESSOR_NAME,
  PLAYBACK_WORKLET_URL,
  SAMPLE_RATE,
} from "../constants/audio";
import {
  H264_CHANNEL_LABEL,
  VIDEO_FPS,
  VIDEO_HEIGHT,
  VIDEO_WIDTH,
} from "../constants/video";

export type OpenCallOptions = {
  video?: boolean;
  camDeviceId?: string | null;
};

export type OpenCall = {
  pc: RTCPeerConnection;
  micStream: MediaStream;
  remoteStream: MediaStream | null;
  localVideoStream: MediaStream | null;
  remoteVideoStream: MediaStream | null;
  close: () => void;
};

export const openCall = async (
  sid: string,
  callId: string,
  micDeviceId: string | null,
  opts: OpenCallOptions = {},
): Promise<OpenCall> => {
  const wantVideo = !!opts.video && videoSupported();
  if (opts.video && !wantVideo) {
    console.warn("video requested but WebCodecs/insertable-streams unsupported; audio only");
  }

  const localStream = await navigator.mediaDevices.getUserMedia({
    audio: micDeviceId ? { deviceId: { exact: micDeviceId } } : true,
    video: wantVideo
      ? {
          deviceId: opts.camDeviceId ? { exact: opts.camDeviceId } : undefined,
          width: { ideal: VIDEO_WIDTH },
          height: { ideal: VIDEO_HEIGHT },
          frameRate: { ideal: VIDEO_FPS },
        }
      : false,
  });

  const pc = new RTCPeerConnection({ iceServers: [] });

  const dc = pc.createDataChannel(PCM_CHANNEL_LABEL, { ordered: true });
  dc.binaryType = "arraybuffer";

  const ctx = new AudioContext({ sampleRate: SAMPLE_RATE });
  await ctx.audioWorklet.addModule(CAPTURE_WORKLET_URL);
  await ctx.audioWorklet.addModule(PLAYBACK_WORKLET_URL);
  await ctx.resume();

  const micSource = ctx.createMediaStreamSource(localStream);
  const captureNode = new AudioWorkletNode(ctx, CAPTURE_PROCESSOR_NAME);
  captureNode.port.onmessage = (e: MessageEvent<Float32Array>) => {
    if (dc.readyState === "open") dc.send(float32ToInt16LE(e.data));
  };
  micSource.connect(captureNode);
  captureNode.connect(ctx.destination);

  const playbackNode = new AudioWorkletNode(ctx, PLAYBACK_PROCESSOR_NAME);
  const streamDest = ctx.createMediaStreamDestination();
  playbackNode.connect(streamDest);
  dc.onmessage = (e: MessageEvent<ArrayBuffer>) => {
    playbackNode.port.postMessage(int16LEToFloat32(e.data));
  };

  let videoSender: VideoSender | null = null;
  let videoReceiver: VideoReceiver | null = null;
  let localVideoStream: MediaStream | null = null;
  let remoteVideoStream: MediaStream | null = null;
  if (wantVideo) {
    const camTrack = localStream.getVideoTracks()[0];
    if (camTrack) {
      localVideoStream = new MediaStream([camTrack]);
      const videoDc = pc.createDataChannel(H264_CHANNEL_LABEL, { ordered: false, maxRetransmits: 0 });
      videoDc.binaryType = "arraybuffer";
      videoReceiver = new VideoReceiver();
      remoteVideoStream = videoReceiver.stream;
      videoDc.onmessage = (e: MessageEvent<ArrayBuffer>) => videoReceiver?.decode(e.data);
      videoDc.onopen = () => {
        videoSender = new VideoSender(camTrack, (au) => {
          if (videoDc.readyState === "open") videoDc.send(au);
        });
      };
    }
  }

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
    remoteStream: streamDest.stream,
    localVideoStream,
    remoteVideoStream,
    close: () => {
      try {
        videoSender?.close();
      } catch {}
      try {
        videoReceiver?.close();
      } catch {}
      try {
        localStream.getTracks().forEach((t) => t.stop());
      } catch {}
      try {
        ctx.close();
      } catch {}
      try {
        pc.close();
      } catch {}
    },
  };
};
