import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import { copyFileSync, mkdirSync, writeFileSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));

// Silero VAD ships its ONNX model + ORT WASM runtime as separate files
// that the library loads at runtime via fetch(). We copy them into the
// build's assets/ directory and tell the library to look there — that
// way the bundle is self-contained and works offline once the binary
// is built (no CDN fetch on first load).
const vadAssets = () => ({
  name: 'copy-vad-assets',
  buildStart() {
    const dest = resolve(__dirname, 'public');
    mkdirSync(dest, { recursive: true });
    const files = [
      'node_modules/@ricky0123/vad-web/dist/vad.worklet.bundle.min.js',
      'node_modules/@ricky0123/vad-web/dist/silero_vad_legacy.onnx',
      'node_modules/@ricky0123/vad-web/dist/silero_vad_v5.onnx',
      'node_modules/onnxruntime-web/dist/ort-wasm-simd-threaded.wasm',
      'node_modules/onnxruntime-web/dist/ort-wasm-simd-threaded.jsep.wasm',
      'node_modules/onnxruntime-web/dist/ort-wasm-simd-threaded.mjs',
    ];
    for (const rel of files) {
      const src = resolve(__dirname, rel);
      try {
        copyFileSync(src, resolve(dest, rel.split('/').pop()!));
      } catch {
        // Some files exist only in some ORT/VAD versions. Missing ones
        // are silently skipped — the library will fall back to whatever
        // it can find at runtime.
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
