<script lang="ts">
  let { onsend, disabled }: { onsend: (text: string) => void; disabled: boolean } = $props();
  let text = $state('');

  function submit() {
    if (!text.trim() || disabled) return;
    onsend(text);
    text = '';
  }

  function onkeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      submit();
    }
  }
</script>

<div class="row">
  <input
    type="text"
    placeholder="Type a message…"
    bind:value={text}
    onkeydown={onkeydown}
    {disabled}
  />
  <button onclick={submit} disabled={disabled || !text.trim()}>Send</button>
</div>

<style>
  .row { display: flex; gap: 0.4rem; }
  input { flex: 1; }
</style>
