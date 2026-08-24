const RING_SIZE = 16000 * 2;
const PRIME_SAMPLES = 16000 * 0.2;
const MAX_SAMPLES = 16000 * 0.6;

class PlaybackProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.ring = new Float32Array(RING_SIZE);
    this.read = 0;
    this.write = 0;
    this.available = 0;
    this.priming = true;
    this.port.onmessage = (e) => {
      const data = e.data;
      for (let i = 0; i < data.length; i += 1) {
        this.ring[this.write] = data[i];
        this.write = (this.write + 1) % RING_SIZE;
        if (this.available < RING_SIZE) {
          this.available += 1;
        } else {
          this.read = (this.read + 1) % RING_SIZE;
        }
      }
      if (this.available > MAX_SAMPLES) {
        const drop = this.available - MAX_SAMPLES;
        this.read = (this.read + drop) % RING_SIZE;
        this.available -= drop;
      }
    };
  }

  process(_inputs, outputs) {
    const out = outputs[0][0];
    if (!out) return true;

    if (this.priming) {
      if (this.available < PRIME_SAMPLES) {
        out.fill(0);
        return true;
      }
      this.priming = false;
    }

    for (let i = 0; i < out.length; i += 1) {
      if (this.available > 0) {
        out[i] = this.ring[this.read];
        this.read = (this.read + 1) % RING_SIZE;
        this.available -= 1;
      } else {
        this.priming = true;
        out[i] = 0;
      }
    }
    return true;
  }
}

registerProcessor("playback-processor", PlaybackProcessor);
