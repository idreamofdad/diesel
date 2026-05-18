<script lang="ts">
  // Silero VAD via @ricky0123/vad-web. The library wraps a Silero ONNX
  // model behind an AudioWorklet: it owns mic capture + VAD detection
  // and gives us callbacks on speech start/end with the recorded PCM.
  //
  // On stop we wrap the PCM in a WAV (16 kHz mono 16-bit signed int LE
  // — same format the Go pipeline expects) and POST to /api/transcribe.
  // The server forwards to STT, the recognized text becomes a new turn
  // through the hub, and the resulting reply lands like any other.
  //
  // The library fetches its ONNX model + ORT runtime relative to the
  // page origin; vite.config.ts copies those files into public/ at
  // build time so they ship with the binary and don't need a CDN.

  import { uploadAudio } from './hub';
  import { MicVAD } from '@ricky0123/vad-web';

  let { disabled }: { disabled: boolean } = $props();
  // Three independent states, deliberately split:
  //   listening — VAD is running and the mic is open. Set by the
  //               click handler and cleared on pause().
  //   capturing — currently inside a speech segment (between
  //               onSpeechStart and onSpeechEnd). Drives the red
  //               recording indicator. Silero's VAD keeps listening
  //               after each segment ends, so this naturally flips
  //               back on for every new utterance — without the
  //               onSpeechStart hook the red state would never come
  //               back for subsequent segments.
  //   busy      — upload to /api/transcribe in flight.
  let listening = $state(false);
  let capturing = $state(false);
  let busy = $state(false);
  let error = $state('');
  let vad: MicVAD | null = null;

  async function ensureVAD(): Promise<MicVAD> {
    if (vad) return vad;
    vad = await MicVAD.new({
      // Asset URLs — match the files vite.config.ts copies into public/.
      baseAssetPath: '/',
      onnxWASMBasePath: '/',
      onSpeechStart: () => {
        capturing = true;
      },
      onSpeechEnd: async (audio: Float32Array) => {
        // VAD fired — wrap as WAV at 16 kHz mono and post for transcription.
        capturing = false;
        busy = true;
        error = '';
        try {
          const wav = encodeWAV(audio, 16000);
          const blob = new Blob([wav], { type: 'audio/wav' });
          await uploadAudio(blob, 'mic.wav');
          // The server pushes the user turn into the hub on our behalf;
          // the WS broadcast will paint the transcript and reply.
        } catch (e) {
          error = (e as Error).message;
        } finally {
          busy = false;
        }
      },
      onVADMisfire: () => {
        // Short blip that didn't hit the speech threshold — clear the
        // capturing state but leave the mic listening for the real one.
        capturing = false;
      },
    });
    return vad;
  }

  async function toggle() {
    // Mid-capture and mid-upload clicks pause the whole loop — the
    // user clearly wants the mic off.
    if (listening) {
      vad?.pause();
      listening = false;
      capturing = false;
      return;
    }
    if (disabled) return;
    try {
      const v = await ensureVAD();
      v.start();
      listening = true;
    } catch (e) {
      error = (e as Error).message;
    }
  }

  // encodeWAV converts the Float32Array Silero hands us (mono, in
  // [-1, 1]) into a 16-bit PCM WAV byte buffer. Sample rate is the
  // VAD's native 16 kHz — matches what whisper-family STT models
  // expect, so no upstream resampling is needed.
  function encodeWAV(samples: Float32Array, sampleRate: number): ArrayBuffer {
    const bytesPerSample = 2;
    const buf = new ArrayBuffer(44 + samples.length * bytesPerSample);
    const view = new DataView(buf);
    let off = 0;
    function writeStr(s: string) {
      for (let i = 0; i < s.length; i++) view.setUint8(off + i, s.charCodeAt(i));
      off += s.length;
    }
    writeStr('RIFF');
    view.setUint32(off, 36 + samples.length * bytesPerSample, true); off += 4;
    writeStr('WAVE');
    writeStr('fmt ');
    view.setUint32(off, 16, true); off += 4;        // PCM chunk size
    view.setUint16(off, 1, true); off += 2;          // PCM format
    view.setUint16(off, 1, true); off += 2;          // mono
    view.setUint32(off, sampleRate, true); off += 4;
    view.setUint32(off, sampleRate * bytesPerSample, true); off += 4;
    view.setUint16(off, bytesPerSample, true); off += 2;
    view.setUint16(off, 16, true); off += 2;         // bits per sample
    writeStr('data');
    view.setUint32(off, samples.length * bytesPerSample, true); off += 4;
    for (let i = 0; i < samples.length; i++) {
      const s = Math.max(-1, Math.min(1, samples[i]));
      view.setInt16(off, s < 0 ? s * 0x8000 : s * 0x7fff, true);
      off += 2;
    }
    return buf;
  }
</script>

<button
  class="mic"
  class:recording
  onclick={toggle}
  disabled={disabled && !recording}
  title={recording ? 'Stop recording' : 'Record (Silero VAD auto-stops on silence)'}
>
  {#if busy}
    …
  {:else if recording}
    ⏹
  {:else}
    ◉
  {/if}
</button>
{#if error}
  <span class="err">{error}</span>
{/if}

<style>
  .mic {
    width: 38px;
    height: 38px;
    border-radius: 50%;
    padding: 0;
    font-size: 18px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
  }
  .mic.recording { background: #7a3a3a; border-color: #a04a4a; }
  .err { color: var(--accent-you); font-size: 0.85rem; margin-left: 0.5rem; }
</style>
