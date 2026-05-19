import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import { copyFileSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));

// Silero VAD ships its ONNX model + ORT WASM runtime as separate files
// that the library loads at runtime via fetch(). We copy them into the
// build's assets/ directory and tell the library to look there — that
// way the bundle is self-contained and works offline once the binary
// is built (no CDN fetch on first load).
//
// Important: @ricky0123/vad-web pins its own (older) onnxruntime-web
// under node_modules/@ricky0123/vad-web/node_modules/. The library
// loads ort-wasm{,-simd,-threaded,-simd-threaded}.wasm at runtime
// based on browser feature detection — copying from the top-level
// onnxruntime-web (newer file naming scheme) leaves the library
// asking for files that don't exist, falling through to index.html,
// and crashing with "expected magic word 00 61 73 6d". We copy from
// the nested install so the names match what the library asks for.
const vadAssets = () => ({
  name: 'copy-vad-assets',
  buildStart() {
    // Wipe public/ first so leftovers from a previous build (e.g.
    // files from an old vite.config that referenced different ORT
    // paths) don't end up in dist/. Vite empties dist/ but happily
    // re-copies whatever's sitting in public/.
    const dest = resolve(__dirname, 'public');
    rmSync(dest, { recursive: true, force: true });
    mkdirSync(dest, { recursive: true });
    const vad = 'node_modules/@ricky0123/vad-web/dist';
    // ORT WASM lives at one of two paths depending on whether npm
    // hoisted vad-web's pinned onnxruntime-web up to the top-level
    // or kept it nested. The hoisted path is the common case; we
    // probe both and copy from whichever exists. All four
    // permutations are needed because the library picks one at
    // runtime based on SharedArrayBuffer + SIMD detection.
    const ortCandidates = [
      'node_modules/onnxruntime-web/dist',
      'node_modules/@ricky0123/vad-web/node_modules/onnxruntime-web/dist',
    ];
    const wasmNames = [
      'ort-wasm.wasm',
      'ort-wasm-simd.wasm',
      'ort-wasm-threaded.wasm',
      'ort-wasm-simd-threaded.wasm',
    ];
    const vadNames = [
      'vad.worklet.bundle.min.js',
      'silero_vad_legacy.onnx',
      'silero_vad_v5.onnx',
    ];
    for (const name of vadNames) {
      try {
        copyFileSync(resolve(__dirname, vad, name), resolve(dest, name));
      } catch {
        /* missing in this VAD version — skip */
      }
    }
    for (const name of wasmNames) {
      let copied = false;
      for (const dir of ortCandidates) {
        try {
          copyFileSync(resolve(__dirname, dir, name), resolve(dest, name));
          copied = true;
          break;
        } catch {
          /* try the next candidate */
        }
      }
      if (!copied) {
        // Hard fail at build time — a missing WASM shows up as a
        // confusing "magic word" error in the browser, but the
        // root cause (the file was never copied) is obvious here.
        throw new Error(`ORT WASM ${name} not found in any of ${ortCandidates.join(', ')}`);
      }
    }
  },
});

// Vite's `emptyOutDir: true` wipes the committed dist/.gitkeep sentinel
// every build. Re-create it in closeBundle so a subsequent `git status`
// stays clean — the embed.go directive needs at least one file in
// dist/ for fresh-clone builds, and .gitkeep is the only thing in
// there that git tracks (everything else is gitignored).
const keepSentinel = () => ({
  name: 'restore-gitkeep',
  closeBundle() {
    writeFileSync(resolve(__dirname, 'dist/.gitkeep'), '');
  },
});

export default defineConfig({
  plugins: [svelte(), vadAssets(), keepSentinel()],
  // SPA built to dist/; Go embeds the whole tree under web/dist.
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // Inline assets up to 4 kB so the network round-trip count stays
    // small — the SPA is tiny enough that one or two files is fine.
    assetsInlineLimit: 4096,
  },
  // Dev server. The Svelte app makes API calls to / which Vite proxies
  // through to the Go server running on :7777, so the dev loop is just
  // `npm run dev` in one terminal and `go run ./cmd/diesel` in another.
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:7777',
        ws: true,
        changeOrigin: true,
      },
    },
  },
});
