export const SAMPLE_RATE = 16000;
export const PCM_CHANNEL_LABEL = "pcm";
export const CAPTURE_WORKLET_URL = "/worklets/capture-processor.js";
export const PLAYBACK_WORKLET_URL = "/worklets/playback-processor.js";
export const CAPTURE_PROCESSOR_NAME = "capture-processor";
export const PLAYBACK_PROCESSOR_NAME = "playback-processor";

// Teto da fila de envio do canal de PCM. Um quarto de segundo de audio a 16 kHz
// em amostras de 16 bits: o que passa disso chegaria tarde para ser tocado, e
// deixar a fila encher faz o send lancar e travar a captura.
export const MAX_BUFFERED_BYTES = 16000 * 2 * 0.25;
