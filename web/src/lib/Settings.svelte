<script lang="ts">
  // Settings.svelte mirrors the desktop Qt settings dialog. It's a
  // tabbed modal that fetches /api/v1/settings on open, lets the user
  // edit every browser-relevant field, and POSTs the result back.
  //
  // Out of scope on purpose:
  //   - Server tab (port / bind / auth token): editing those from the
  //     web could lock the user out of the running connection.
  //   - Input/Output device pickers: those are host-side hardware,
  //     not the browser's audio devices.
  // Both are preserved untouched by the server's merge logic.

  import { onMount, onDestroy } from 'svelte';
  import {
    fetchSettings,
    saveSettings,
    probeModels,
    testConnection,
    testTTS,
    authToken,
    SECRET_MASK,
    type AppSettings,
    type ProbeBody,
  } from './hub';

  let { onclose }: { onclose: () => void } = $props();

  type Tab = 'llm' | 'stt' | 'tts' | 'image' | 'appearance';
  let tab = $state<Tab>('llm');

  let settings = $state<AppSettings | null>(null);
  let loadError = $state('');
  let saving = $state(false);
  let saveError = $state('');

  let token = $state('');
  authToken.subscribe(v => { token = v; });

  // Per-service async state. Each service owns its own model list,
  // probe state, and status text — same shape as the desktop dialog
  // where each tab owns its loader.
  let llmModels = $state<string[]>([]);
  let llmStatus = $state('');
  let llmTesting = $state(false);
  let llmContext = $state<number | null>(null);

  let sttModels = $state<string[]>([]);
  let sttStatus = $state('');
  let sttTesting = $state(false);

  let ttsModels = $state<string[]>([]);
  let ttsStatus = $state('');
  let ttsTesting = $state(false);
  let ttsAudioURL = $state('');

  let imgStatus = $state('');
  let imgTesting = $state(false);

  // Debounce timers so typing in an endpoint/key field doesn't spam
  // the upstream provider on every keystroke. Mirrors the 400 ms
  // QTimer the desktop dialog uses.
  let llmTimer: ReturnType<typeof setTimeout> | null = null;
  let sttTimer: ReturnType<typeof setTimeout> | null = null;
  let ttsTimer: ReturnType<typeof setTimeout> | null = null;

  function debounce(slot: 'llm' | 'stt' | 'tts', fn: () => void, ms = 400) {
    const current = slot === 'llm' ? llmTimer : slot === 'stt' ? sttTimer : ttsTimer;
    if (current) clearTimeout(current);
    const t = setTimeout(fn, ms);
    if (slot === 'llm') llmTimer = t;
    else if (slot === 'stt') sttTimer = t;
    else ttsTimer = t;
  }

  onMount(async () => {
    try {
      settings = await fetchSettings();
      // Kick off model fetches for every service so the dropdowns
      // are populated by the time the user clicks into the tab.
      refreshLLMModels();
      refreshSTTModels();
      refreshTTSModels();
    } catch (e) {
      loadError = (e as Error).message;
    }
  });

  onDestroy(() => {
    if (llmTimer) clearTimeout(llmTimer);
    if (sttTimer) clearTimeout(sttTimer);
    if (ttsTimer) clearTimeout(ttsTimer);
    if (ttsAudioURL) URL.revokeObjectURL(ttsAudioURL);
  });

  function probeBody(kind: ProbeBody['kind']): ProbeBody {
    const s = settings!;
    switch (kind) {
      case 'llm':
        return { kind, endpoint: s.api_endpoint, api_key: s.api_key, model: s.model };
      case 'stt':
        return { kind, endpoint: s.stt_endpoint, api_key: s.stt_api_key };
      case 'tts':
        return { kind, endpoint: s.tts_endpoint, api_key: s.tts_api_key, model: s.tts_model, voice: s.tts_voice };
      case 'image':
        return { kind, endpoint: s.comfyui_endpoint };
    }
  }

  async function refreshLLMModels() {
    if (!settings) return;
    try {
      const res = await probeModels(probeBody('llm'));
      llmModels = res.models;
      if (res.context_length !== undefined) llmContext = res.context_length;
    } catch {
      llmModels = [];
    }
  }

  async function refreshSTTModels() {
    if (!settings) return;
    try {
      const res = await probeModels(probeBody('stt'));
      sttModels = res.models;
    } catch {
      sttModels = [];
    }
  }

  async function refreshTTSModels() {
    if (!settings) return;
    try {
      const res = await probeModels(probeBody('tts'));
      ttsModels = res.models;
    } catch {
      ttsModels = [];
    }
  }

  async function runTest(kind: ProbeBody['kind']) {
    if (!settings) return;
    if (kind === 'llm') { llmTesting = true; llmStatus = 'Testing…'; }
    if (kind === 'stt') { sttTesting = true; sttStatus = 'Testing…'; }
    if (kind === 'image') { imgTesting = true; imgStatus = 'Testing…'; }
    try {
      const status = await testConnection(probeBody(kind));
      if (kind === 'llm') { llmStatus = status; if (status.startsWith('✓')) refreshLLMModels(); }
      if (kind === 'stt') { sttStatus = status; if (status.startsWith('✓')) refreshSTTModels(); }
      if (kind === 'image') imgStatus = status;
    } catch (e) {
      const msg = '✗ ' + (e as Error).message;
      if (kind === 'llm') llmStatus = msg;
      if (kind === 'stt') sttStatus = msg;
      if (kind === 'image') imgStatus = msg;
    } finally {
      if (kind === 'llm') llmTesting = false;
      if (kind === 'stt') sttTesting = false;
      if (kind === 'image') imgTesting = false;
    }
  }

  async function runTTSTest() {
    if (!settings) return;
    ttsTesting = true;
    ttsStatus = 'Synthesizing…';
    if (ttsAudioURL) {
      URL.revokeObjectURL(ttsAudioURL);
      ttsAudioURL = '';
    }
    try {
      const blob = await testTTS(probeBody('tts'));
      ttsAudioURL = URL.createObjectURL(blob);
      const audio = new Audio(ttsAudioURL);
      audio.play().catch(() => { /* autoplay blocked — user can hit Play button */ });
      ttsStatus = '✓ Synthesized — playing sample.';
    } catch (e) {
      ttsStatus = '✗ ' + (e as Error).message;
    } finally {
      ttsTesting = false;
    }
  }

  function estimateTokens(text: string): number {
    // Same chars/4 heuristic the Go side uses (settings.EstimateTokens)
    // so the displayed count matches between the two UIs.
    const trimmed = text.trim();
    if (!trimmed) return 0;
    return Math.ceil([...trimmed].length / 4);
  }

  function saveToken() {
    authToken.set(token.trim());
  }

  async function save() {
    if (!settings) return;
    saving = true;
    saveError = '';
    try {
      // Save the auth token first in case the user changed it — the
      // settings POST itself needs to be authorized with the *new*
      // token if the user just rotated it, but practically the more
      // common flow is "I just set the token for the first time".
      authToken.set(token.trim());
      settings = await saveSettings(settings);
      onclose();
    } catch (e) {
      saveError = (e as Error).message;
    } finally {
      saving = false;
    }
  }

  function cancel() {
    onclose();
  }

  function onBackdrop(e: MouseEvent) {
    if (e.target === e.currentTarget) cancel();
  }
</script>

<div class="backdrop" onclick={onBackdrop} role="presentation">
  <div class="modal" role="dialog" aria-modal="true" aria-label="Settings">
    <header>
      <h2>Settings</h2>
      <button class="x" onclick={cancel} title="Close">✕</button>
    </header>

    {#if loadError}
      <div class="error">Couldn't load settings: {loadError}</div>
      <div class="auth-row">
        <label>
          Auth token:
          <input type="password" bind:value={token} placeholder="(blank if server has no auth)" />
        </label>
        <button onclick={saveToken}>Save token & retry</button>
      </div>
    {:else if !settings}
      <div class="loading">Loading…</div>
    {:else}
      <nav class="tabs">
        <button class:active={tab === 'llm'} onclick={() => (tab = 'llm')}>LLM</button>
        <button class:active={tab === 'stt'} onclick={() => (tab = 'stt')}>Speech-to-Text</button>
        <button class:active={tab === 'tts'} onclick={() => (tab = 'tts')}>Text-to-Speech</button>
        <button class:active={tab === 'image'} onclick={() => (tab = 'image')}>Image Generation</button>
        <button class:active={tab === 'appearance'} onclick={() => (tab = 'appearance')}>Appearance</button>
      </nav>

      <div class="body">
        {#if tab === 'llm'}
          <div class="form">
            <label>API endpoint
              <input
                bind:value={settings.api_endpoint}
                oninput={() => debounce('llm', refreshLLMModels)}
              />
            </label>
            <label>API key
              <input
                type="password"
                bind:value={settings.api_key}
                placeholder="sk-…"
                oninput={() => debounce('llm', refreshLLMModels)}
              />
              {#if settings.api_key === SECRET_MASK}
                <small class="hint">A saved key is in use. Type to replace it.</small>
              {/if}
            </label>
            <label>Model
              <input
                bind:value={settings.model}
                list="llm-model-list"
                oninput={() => debounce('llm', refreshLLMModels, 600)}
              />
              <datalist id="llm-model-list">
                {#each llmModels as id}
                  <option value={id}></option>
                {/each}
              </datalist>
            </label>
            <label>System prompt
              <textarea bind:value={settings.system_prompt} rows="8"></textarea>
              <small class="hint right">~{estimateTokens(settings.system_prompt)} tokens</small>
            </label>
            <div class="kv">
              <span>Context length:</span>
              <span class="muted">{llmContext === null ? '—' : llmContext > 0 ? `${llmContext} tokens` : 'not reported by this server'}</span>
            </div>
            <label>Message history
              <input type="number" min="0" max="500" bind:value={settings.history_messages} />
            </label>
            <div class="test-row">
              <button onclick={() => runTest('llm')} disabled={llmTesting}>Test connection</button>
              <span class="status">{llmStatus}</span>
            </div>
          </div>
        {:else if tab === 'stt'}
          <div class="form">
            <label>Endpoint
              <input
                bind:value={settings.stt_endpoint}
                placeholder="(falls back to API endpoint)"
                oninput={() => debounce('stt', refreshSTTModels)}
              />
            </label>
            <label>API key
              <input
                type="password"
                bind:value={settings.stt_api_key}
                placeholder="(falls back to API key)"
                oninput={() => debounce('stt', refreshSTTModels)}
              />
              {#if settings.stt_api_key === SECRET_MASK}
                <small class="hint">A saved key is in use. Type to replace it.</small>
              {/if}
            </label>
            <label>Model
              <input bind:value={settings.stt_model} list="stt-model-list" placeholder="whisper-1" />
              <datalist id="stt-model-list">
                {#each sttModels as id}
                  <option value={id}></option>
                {/each}
              </datalist>
            </label>
            <label class="check">
              <input type="checkbox" bind:checked={settings.continuous_conversation} />
              Continuous conversation (keep listening after each reply)
            </label>
            <div class="test-row">
              <button onclick={() => runTest('stt')} disabled={sttTesting}>Test connection</button>
              <span class="status">{sttStatus}</span>
            </div>
          </div>
        {:else if tab === 'tts'}
          <div class="form">
            <label class="check">
              <input type="checkbox" bind:checked={settings.enable_tts} />
              Speak replies through TTS
            </label>
            <label>Endpoint
              <input
                bind:value={settings.tts_endpoint}
                placeholder="(falls back to API endpoint)"
                oninput={() => debounce('tts', refreshTTSModels)}
              />
            </label>
            <label>API key
              <input
                type="password"
                bind:value={settings.tts_api_key}
                placeholder="(falls back to API key)"
                oninput={() => debounce('tts', refreshTTSModels)}
              />
              {#if settings.tts_api_key === SECRET_MASK}
                <small class="hint">A saved key is in use. Type to replace it.</small>
              {/if}
            </label>
            <label>Model
              <input bind:value={settings.tts_model} list="tts-model-list" placeholder="tts-1" />
              <datalist id="tts-model-list">
                {#each ttsModels as id}
                  <option value={id}></option>
                {/each}
              </datalist>
            </label>
            <label>Voice
              <input bind:value={settings.tts_voice} placeholder="alloy" />
            </label>
            <div class="test-row">
              <button onclick={runTTSTest} disabled={ttsTesting}>Test voice</button>
              <span class="status">{ttsStatus}</span>
            </div>
          </div>
        {:else if tab === 'image'}
          <div class="form">
            <label class="check">
              <input type="checkbox" bind:checked={settings.enable_image_gen} />
              Render a character portrait after each reply
            </label>
            <label>ComfyUI endpoint
              <input bind:value={settings.comfyui_endpoint} placeholder="http://127.0.0.1:8188" />
            </label>
            <label>Steps
              <input type="number" min="1" max="200" bind:value={settings.image_steps} />
            </label>
            <label>Image prompt
              <textarea bind:value={settings.image_prompt} rows="6"></textarea>
            </label>
            <label>Clothing
              <textarea bind:value={settings.image_clothing} rows="2"></textarea>
            </label>
            <label>Nudity
              <textarea bind:value={settings.image_nudity} rows="2"></textarea>
            </label>
            <label>Negative prompt
              <textarea bind:value={settings.image_negative_prompt} rows="4"></textarea>
            </label>
            <div class="test-row">
              <button onclick={() => runTest('image')} disabled={imgTesting}>Test connection</button>
              <span class="status">{imgStatus}</span>
            </div>
          </div>
        {:else if tab === 'appearance'}
          <div class="form">
            <label>Theme
              <select bind:value={settings.theme}>
                <option>System</option>
                <option>Dark</option>
                <option>Light</option>
              </select>
              <small class="hint">Theme is honored by the desktop app; the web UI is dark-only today.</small>
            </label>
            <label class="check">
              <input type="checkbox" bind:checked={settings.save_to_disk} />
              Save conversations to disk
            </label>
            <hr />
            <label>Web client auth token
              <input type="password" bind:value={token} placeholder="(blank if server has no auth)" />
              <small class="hint">Stored in this browser only. The server's token is set in the desktop app's Server tab.</small>
            </label>
          </div>
        {/if}
      </div>

      <footer>
        {#if saveError}<span class="error inline">✗ {saveError}</span>{/if}
        <div class="spacer"></div>
        <button onclick={cancel} disabled={saving}>Cancel</button>
        <button class="primary" onclick={save} disabled={saving}>{saving ? 'Saving…' : 'Save'}</button>
      </footer>
    {/if}
  </div>
</div>

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.6);
    z-index: 1000;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 2rem;
  }
  .modal {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 8px;
    width: min(640px, 100%);
    max-height: 90vh;
    display: flex;
    flex-direction: column;
    box-shadow: 0 18px 60px rgba(0, 0, 0, 0.45);
  }
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 0.75rem 1rem;
    border-bottom: 1px solid var(--border);
  }
  header h2 { margin: 0; font-size: 1.05rem; }
  .x {
    background: transparent;
    border: 0;
    color: var(--muted);
    padding: 0.2rem 0.5rem;
    font-size: 1.1rem;
  }
  .x:hover { color: var(--text); background: transparent; }

  .tabs {
    display: flex;
    gap: 0;
    border-bottom: 1px solid var(--border);
    overflow-x: auto;
  }
  .tabs button {
    background: transparent;
    border: 0;
    border-bottom: 2px solid transparent;
    border-radius: 0;
    padding: 0.6rem 0.9rem;
    color: var(--muted);
    white-space: nowrap;
  }
  .tabs button.active {
    color: var(--text);
    border-bottom-color: var(--accent-them);
  }
  .tabs button:hover:not(:disabled) { background: transparent; color: var(--text); }

  .body {
    flex: 1 1 auto;
    overflow-y: auto;
    padding: 1rem;
  }
  .form { display: flex; flex-direction: column; gap: 0.85rem; }
  .form label {
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
    font-size: 0.9rem;
    color: var(--muted);
  }
  .form label.check {
    flex-direction: row;
    align-items: center;
    gap: 0.5rem;
    color: var(--text);
  }
  .form input[type="checkbox"] { width: auto; margin: 0; flex: none; }
  .form input, .form textarea, .form select {
    width: 100%;
    color: var(--text);
    background: var(--panel);
    border: 1px solid var(--border);
    border-radius: 4px;
    padding: 0.4rem 0.5rem;
    font: inherit;
  }
  .form textarea { resize: vertical; font-family: inherit; }
  .form select { padding: 0.35rem 0.5rem; }
  .hint { color: var(--muted); font-size: 0.78rem; }
  .hint.right { align-self: flex-end; }
  .kv {
    display: flex;
    justify-content: space-between;
    font-size: 0.9rem;
  }
  .kv .muted { color: var(--muted); }

  .test-row {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    margin-top: 0.25rem;
  }
  .test-row .status {
    flex: 1;
    font-size: 0.85rem;
    color: var(--muted);
    white-space: pre-wrap;
  }

  hr { width: 100%; border: 0; border-top: 1px solid var(--border); margin: 0.25rem 0; }

  footer {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.75rem 1rem;
    border-top: 1px solid var(--border);
  }
  .spacer { flex: 1; }
  .primary {
    background: var(--accent-them);
    border-color: var(--accent-them);
    color: #0a1c2e;
  }
  .primary:hover:not(:disabled) {
    background: #74b7ff;
  }

  .error {
    color: #e57373;
    padding: 0.75rem 1rem;
  }
  .error.inline {
    padding: 0;
    font-size: 0.85rem;
  }
  .loading {
    padding: 2rem 1rem;
    text-align: center;
    color: var(--muted);
  }
  .auth-row {
    display: flex;
    gap: 0.5rem;
    align-items: end;
    padding: 0 1rem 1rem;
  }
  .auth-row label {
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
    color: var(--muted);
    font-size: 0.9rem;
  }
</style>
