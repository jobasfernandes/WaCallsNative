import { float32ToInt16LE, int16LEToFloat32 } from "@/lib/pcm";
import {
  CAPTURE_PROCESSOR_NAME,
  CAPTURE_WORKLET_URL,
  PCM_CHANNEL_LABEL,
  PLAYBACK_PROCESSOR_NAME,
  PLAYBACK_WORKLET_URL,
  MAX_BUFFERED_BYTES,
  SAMPLE_RATE,
} from "@/constants/audio";

export type AudioChannel = {
  remoteStream: MediaStream;
  close: () => void;
};

export const setupAudioChannel = async (
  pc: RTCPeerConnection,
  micStream: MediaStream,
): Promise<AudioChannel> => {
  const dc = pc.createDataChannel(PCM_CHANNEL_LABEL, { ordered: true });
  dc.binaryType = "arraybuffer";

  const ctx = new AudioContext({ sampleRate: SAMPLE_RATE });
  await ctx.audioWorklet.addModule(CAPTURE_WORKLET_URL);
  await ctx.audioWorklet.addModule(PLAYBACK_WORKLET_URL);
  await ctx.resume();

  const micSource = ctx.createMediaStreamSource(micStream);
  const captureNode = new AudioWorkletNode(ctx, CAPTURE_PROCESSOR_NAME);
  captureNode.port.onmessage = (e: MessageEvent<Float32Array>) => {
    if (dc.readyState !== "open") return;
    // Enfileirar audio que nao cabe nao adianta: ele chegaria tarde demais para
    // ser tocado. Pior, a fila cheia faz o send lancar, e como o canal e
    // ordenado o congestionamento trava tambem o que vem na outra direcao.
    if (dc.bufferedAmount > MAX_BUFFERED_BYTES) return;
    try {
      dc.send(float32ToInt16LE(e.data));
    } catch {
      // Uma excecao aqui derruba o handler de captura e o microfone para de
      // subir de vez.
    }
  };
  micSource.connect(captureNode);
  captureNode.connect(ctx.destination);

  const playbackNode = new AudioWorkletNode(ctx, PLAYBACK_PROCESSOR_NAME);
  const streamDest = ctx.createMediaStreamDestination();
  playbackNode.connect(streamDest);
  dc.onmessage = (e: MessageEvent<ArrayBuffer>) => {
    playbackNode.port.postMessage(int16LEToFloat32(e.data));
  };

  return {
    remoteStream: streamDest.stream,
    close: () => {
      try {
        ctx.close();
      } catch {}
    },
  };
};
