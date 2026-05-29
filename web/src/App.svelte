<script lang="ts">
  import { onMount } from 'svelte';
  import {
    connect,
    history,
    statusText,
    inFlight,
    portraitURL,
    connected,
    usage,
    sendMessage,
    muted,
    identityConfigured,
    fetchSettings,
  } from './lib/hub';
  import Transcript from './lib/Transcript.svelte';
  import ChatInput from './lib/ChatInput.svelte';
  import MicButton from './lib/MicButton.svelte';
  import Portrait from './lib/Portrait.svelte';
  import Settings from './lib/Settings.svelte';

  let messages = $state<any[]>([]);
  let status = $state('Connecting…');
  let busy = $state(false);
  let portrait = $state('');
  let online = $state(false);
  let tokens = $state<{ prompt_tokens?: number; completion_tokens?: number; total_tokens?: number }>({});
  let mutedNow = $state(false);
  let showSettings = $state(false);
  let identityOK = $state(false);

  // Wire the imperative stores to local $state — the templates can't
  // bind directly to a custom Writable, so we mirror via subscribe.
  onMount(() => {
    const unsubs = [
      history.subscribe(v => { messages = v; }),
      statusText.subscribe(v => { status = v; }),
      inFlight.subscribe(v => { busy = v; }),
      portraitURL.subscribe(v => { portrait = v; }),
      connected.subscribe(v => { online = v; }),
      usage.subscribe(v => { tokens = v; }),
      muted.subscribe(v => { mutedNow = v; }),
      identityConfigured.subscribe(v => { identityOK = v; }),
    ];
    connect();
    // Seed identityConfigured on load — the WebSocket protocol doesn't
    // carry settings, so the only way to know whether Send should be
    // gated is to hit the REST endpoint once.
    fetchSettings().catch(() => {});
    return () => unsubs.forEach(u => u());
  });

  function tokensSummary() {
    const t = tokens.total_tokens || (tokens.prompt_tokens || 0) + (tokens.completion_tokens || 0);
    if (!t) return '';
    return `${messages.length} msgs · ${t} tokens`;
  }

  function handleSend(text: string) {
    sendMessage(text);
  }

  function toggleMute() {
    muted.set(!mutedNow);
  }

  function closeSettings() {
    showSettings = false;
    // Reconnect so any auth-token change picked up by the dialog
    // takes effect on the WebSocket immediately.
    connect();
  }
</script>

<main>
  <header>
    <div class="title">Diesel</div>
    <div class="actions">
      <span class="conn" class:online>{online ? '● connected' : '○ disconnected'}</span>
      <button onclick={toggleMute} title={mutedNow ? 'Unmute replies' : 'Mute replies'}>
        {mutedNow ? '🔇' : '🔊'}
      </button>
      <button onclick={() => (showSettings = !showSettings)} title="Settings">⚙</button>
    </div>
  </header>

  {#if showSettings}
    <Settings onclose={closeSettings} />
  {/if}

  <section class="body">
    <div class="left">
      <Transcript {messages} />
      <ChatInput onsend={handleSend} disabled={busy || !identityOK} />
      {#if !identityOK}
        <small class="identity-hint">Set first, last, and pet name in Settings before chatting.</small>
      {/if}
      <div class="mic-row">
        <MicButton disabled={busy || !identityOK} />
      </div>
    </div>
    <div class="right">
      <Portrait src={portrait} />
    </div>
  </section>

  <footer>
    <span class="status">{status}</span>
    <span class="tokens">{tokensSummary()}</span>
  </footer>
</main>

<style>
  main {
    display: flex;
    flex-direction: column;
    height: 100vh;
  }
  header {
    flex: 0 0 auto;
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 0.65rem 1rem;
    border-bottom: 1px solid var(--border);
  }
  .title { font-weight: bold; }
  .actions { display: flex; gap: 0.5rem; align-items: center; }
  .conn { font-size: 0.85rem; color: var(--muted); }
  .conn.online { color: #6ec46e; }

  .body {
    flex: 1 1 auto;
    display: grid;
    grid-template-columns: 1fr 320px;
    gap: 0.75rem;
    padding: 0.75rem 1rem;
    min-height: 0;
  }
  .left {
    display: grid;
    grid-template-rows: 1fr auto auto;
    gap: 0.5rem;
    min-height: 0;
  }
  .right { display: flex; flex-direction: column; }
  .mic-row { display: flex; gap: 0.5rem; }

  footer {
    flex: 0 0 auto;
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 0.5rem 1rem;
    border-top: 1px solid var(--border);
    font-size: 0.85rem;
    color: var(--muted);
  }
  .status {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .identity-hint {
    font-size: 0.8rem;
    color: var(--muted);
    padding: 0 0.25rem;
  }
</style>
