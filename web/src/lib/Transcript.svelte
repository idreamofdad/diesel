<script lang="ts">
  import type { Message } from './hub';

  let { messages }: { messages: Message[] } = $props();
  let container: HTMLDivElement;

  // Auto-scroll to bottom when new messages arrive — same UX as the
  // Qt transcript widget, which calls EnsureCursorVisible after every
  // Append. We compare the current scroll position to the bottom and
  // only scroll if the user is already near the end, so a user who
  // scrolled up to re-read something isn't yanked back down. The first
  // render with messages always jumps to the bottom so a restored
  // conversation opens on the latest reply instead of the oldest.
  let prevCount = 0;
  let initialScrolled = false;
  $effect(() => {
    if (!container) return;
    if (messages.length === prevCount) return;
    const firstPaint = !initialScrolled && messages.length > 0;
    prevCount = messages.length;
    const nearBottom = container.scrollHeight - container.scrollTop - container.clientHeight < 80;
    if (firstPaint || nearBottom) {
      initialScrolled = initialScrolled || firstPaint;
      requestAnimationFrame(() => {
        container.scrollTop = container.scrollHeight;
      });
    }
  });
</script>

<div class="transcript" bind:this={container}>
  {#if messages.length === 0}
    <div class="empty">(Conversation will appear here)</div>
  {:else}
    {#each messages as m, i (i)}
      <div class="line">
        <span class="speaker" class:user={m.role === 'user'} class:assistant={m.role === 'assistant'}>
          {m.role === 'user' ? 'You' : 'Diesel'}:
        </span>
        <span class="body">{m.content}</span>
      </div>
    {/each}
  {/if}
</div>

<style>
  .transcript {
    background: var(--panel);
    border: 1px solid var(--border);
    border-radius: 4px;
    padding: 0.75rem;
    overflow-y: auto;
    line-height: 1.45;
    white-space: pre-wrap;
    word-wrap: break-word;
    /* min-height: 0 lets the transcript shrink inside its grid row
       and overflow-scroll instead of pushing the layout taller —
       without it, a long transcript would push the chat input and
       footer off-screen. */
    min-height: 0;
  }
  .empty {
    color: var(--muted);
    font-style: italic;
  }
  .line + .line { margin-top: 0.6rem; }
  .speaker { font-weight: bold; margin-right: 0.35rem; }
  .speaker.user { color: var(--accent-you); }
  .speaker.assistant { color: var(--accent-them); }
</style>
