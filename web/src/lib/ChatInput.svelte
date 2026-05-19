<script lang="ts">
  let { onsend, disabled }: { onsend: (text: string) => void; disabled: boolean } = $props();
  let text = $state('');
  let input: HTMLInputElement;
  // Disabling the input while a reply is in flight clears focus; flip
  // this on submit so we restore focus the moment the field re-enables.
  let refocusOnEnable = false;

  function submit() {
    if (!text.trim() || disabled) return;
    onsend(text);
    text = '';
    refocusOnEnable = true;
  }

  function onkeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      submit();
    }
  }

  $effect(() => {
    if (!disabled && refocusOnEnable && input) {
      refocusOnEnable = false;
      input.focus();
    }
  });
</script>

<div class="row">
  <input
    type="text"
    placeholder="Type a message…"
    bind:value={text}
    bind:this={input}
    onkeydown={onkeydown}
    {disabled}
  />
  <button onclick={submit} disabled={disabled || !text.trim()}>Send</button>
</div>

<style>
  .row { display: flex; gap: 0.4rem; }
  input { flex: 1; }
</style>
