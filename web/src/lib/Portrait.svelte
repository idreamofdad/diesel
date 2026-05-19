<script lang="ts">
  let { src }: { src: string } = $props();
  let modal = $state(false);
</script>

<div class="portrait">
  <div class="header">Diesel</div>
  {#if src}
    <button class="thumb" onclick={() => (modal = true)} title="Click to enlarge">
      <img alt="Diesel portrait" {src} />
    </button>
  {:else}
    <div class="placeholder">(no portrait yet)</div>
  {/if}
</div>

{#if modal && src}
  <button class="modal" onclick={() => (modal = false)}>
    <img alt="Diesel portrait full size" {src} />
  </button>
{/if}

<style>
  .portrait {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }
  .header { font-weight: bold; }
  .thumb {
    background: var(--panel);
    border: 1px solid var(--border);
    border-radius: 4px;
    padding: 0;
    cursor: zoom-in;
    overflow: hidden;
    width: 100%;
  }
  .thumb:hover { background: var(--panel); }
  .thumb img {
    display: block;
    width: 100%;
    height: auto;
  }
  .placeholder {
    background: var(--panel);
    border: 1px solid var(--border);
    border-radius: 4px;
    padding: 3rem 1rem;
    color: var(--muted);
    text-align: center;
  }
  .modal {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.92);
    border: 0;
    padding: 0;
    z-index: 1000;
    cursor: zoom-out;
    display: flex;
    align-items: center;
    justify-content: center;
  }
  .modal img {
    max-width: 100vw;
    max-height: 100vh;
    object-fit: contain;
  }
</style>
