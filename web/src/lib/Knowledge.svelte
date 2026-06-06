<script lang="ts">
  // Knowledge.svelte is the browser editor for Diesel's knowledge graph —
  // the same entities, observations, and relations the model edits through
  // its MCP tools, and the desktop "Knowledge…" dialog edits natively. The
  // left pane lists entities; selecting one shows its observations and the
  // relations touching it, each with add/delete controls. Every mutation
  // writes straight through the REST API and reloads, so the next turn's
  // system-prompt injection reflects the change.

  import { onMount } from 'svelte';
  import {
    fetchGraph,
    createEntity,
    editEntity,
    deleteEntity,
    addObservation,
    editObservation,
    deleteObservation,
    createRelation,
    editRelation,
    deleteRelation,
    type KnowledgeGraph,
    type KGEntity,
    type KGRelation,
  } from './hub';

  let { onclose }: { onclose: () => void } = $props();

  let graph = $state<KnowledgeGraph>({ entities: [], relations: [] });
  let loadError = $state('');
  let actionError = $state('');
  let selectedName = $state('');

  // New-entity form.
  let newName = $state('');
  let newType = $state('');
  // Per-selection add forms.
  let newObs = $state('');
  let relType = $state('');
  let relTarget = $state('');
  // Inline observation editor: editingObs holds the original text of the row
  // being edited (its key), editText the in-progress replacement.
  let editingObs = $state('');
  let editText = $state('');
  // Inline relation editor: editingRel holds the original triple being edited,
  // with relEditType/relEditTarget the in-progress values. Edits keep the
  // original direction (selected entity stays the from/to it already was).
  let editingRel = $state<KGRelation | null>(null);
  let relEditType = $state('');
  let relEditTarget = $state('');
  // Inline entity editor (rename/retype the selected entity).
  let editingEntity = $state(false);
  let entName = $state('');
  let entType = $state('');

  const selected = $derived(graph.entities.find(e => e.name === selectedName) ?? null);
  const touching = $derived(
    graph.relations.filter(r => selected && (r.from === selected.name || r.to === selected.name)),
  );
  const otherNames = $derived(
    graph.entities.filter(e => !selected || e.name !== selected.name).map(e => e.name).sort(),
  );

  async function reload() {
    try {
      graph = await fetchGraph();
      loadError = '';
      // Keep the relation-target dropdown pointed at something valid.
      if (!relTarget || !otherNames.includes(relTarget)) relTarget = otherNames[0] ?? '';
    } catch (e) {
      loadError = (e as Error).message;
    }
  }

  onMount(reload);

  // run wraps a mutation: clear the prior error, perform it, reload, and
  // surface any domain error (e.g. dangling relation) without closing.
  async function run(fn: () => Promise<void>) {
    actionError = '';
    try {
      await fn();
      await reload();
    } catch (e) {
      actionError = (e as Error).message;
    }
  }

  function select(e: KGEntity) {
    selectedName = e.name;
    actionError = '';
    newObs = '';
    relType = '';
    relTarget = otherNames[0] ?? '';
    cancelEdit();
    cancelRelEdit();
    cancelEntityEdit();
  }

  async function addEntity() {
    const name = newName.trim();
    if (!name) return;
    const type = newType.trim();
    await run(() => createEntity(name, type));
    selectedName = name;
    newName = '';
    newType = '';
  }

  async function removeEntity(name: string) {
    if (!confirm(`Delete "${name}" and all its observations and relations?`)) return;
    await run(() => deleteEntity(name));
    if (selectedName === name) selectedName = '';
  }

  function startEntityEdit() {
    if (!selected) return;
    editingEntity = true;
    entName = selected.name;
    entType = selected.entityType;
    actionError = '';
  }

  function cancelEntityEdit() {
    editingEntity = false;
    entName = '';
    entType = '';
  }

  async function saveEntity() {
    if (!selected) return;
    const name = entName.trim();
    const type = entType.trim();
    if (!name) return;
    if (name === selected.name && type === selected.entityType) {
      cancelEntityEdit();
      return;
    }
    const oldName = selected.name;
    await run(() => editEntity(oldName, name, type));
    selectedName = name;
    cancelEntityEdit();
  }

  async function addObs() {
    const content = newObs.trim();
    if (!content || !selected) return;
    const entity = selected.name;
    await run(() => addObservation(entity, content));
    newObs = '';
  }

  function startEdit(obs: string) {
    editingObs = obs;
    editText = obs;
    actionError = '';
  }

  function cancelEdit() {
    editingObs = '';
    editText = '';
  }

  // saveObs rewrites the edited observation in place. Unchanged or empty text
  // just closes the editor; the original text keys the row server-side.
  async function saveObs() {
    const orig = editingObs;
    const next = editText.trim();
    if (!orig || !selected) return;
    if (!next || next === orig) {
      cancelEdit();
      return;
    }
    const entity = selected.name;
    await run(() => editObservation(entity, orig, next));
    cancelEdit();
  }

  async function addRel() {
    const rt = relType.trim();
    if (!rt || !relTarget || !selected) return;
    const from = selected.name;
    await run(() => createRelation(from, relTarget, rt));
    relType = '';
  }

  function relText(r: KGRelation): string {
    return `${r.from}  —${r.relationType}→  ${r.to}`;
  }

  function relKey(r: KGRelation): string {
    return `${r.from} ${r.relationType} ${r.to}`;
  }

  function startRelEdit(r: KGRelation) {
    editingRel = r;
    relEditType = r.relationType;
    relEditTarget = selected && r.from === selected.name ? r.to : r.from;
    actionError = '';
  }

  function cancelRelEdit() {
    editingRel = null;
    relEditType = '';
    relEditTarget = '';
  }

  async function saveRel() {
    const orig = editingRel;
    const rt = relEditType.trim();
    if (!orig || !selected || !rt || !relEditTarget) return;
    // Preserve direction: the selected entity stays the endpoint it already
    // was, relEditTarget replaces the other one.
    const outgoing = orig.from === selected.name;
    const next: KGRelation = outgoing
      ? { from: selected.name, to: relEditTarget, relationType: rt }
      : { from: relEditTarget, to: selected.name, relationType: rt };
    if (next.from === orig.from && next.to === orig.to && next.relationType === orig.relationType) {
      cancelRelEdit();
      return;
    }
    await run(() => editRelation(orig, next));
    cancelRelEdit();
  }

  // Close only when the click lands on the backdrop itself, not when it
  // bubbles up from inside the modal — the same pattern Settings uses.
  function onBackdrop(e: MouseEvent) {
    if (e.target === e.currentTarget) onclose();
  }
</script>

<div class="backdrop" onclick={onBackdrop} role="presentation">
  <div class="modal" role="dialog" aria-modal="true" aria-label="Knowledge graph">
    <header>
      <h2>Knowledge graph</h2>
      <button class="x" onclick={onclose} aria-label="Close">✕</button>
    </header>

    {#if loadError}
      <div class="body"><p class="error">{loadError}</p></div>
    {:else}
      <div class="addbar">
        <input placeholder="Name, e.g. Tyr Mactire" bind:value={newName} />
        <input placeholder="Type, e.g. person" bind:value={newType} />
        <button onclick={addEntity} disabled={!newName.trim()}>Add entity</button>
        <button class="ghost" onclick={reload} title="Reload">⟳</button>
      </div>

      {#if actionError}
        <p class="error">{actionError}</p>
      {/if}

      <div class="cols">
        <ul class="entities">
          {#if graph.entities.length === 0}
            <li class="muted">No entities yet.</li>
          {/if}
          {#each graph.entities as e (e.name)}
            <li class:active={e.name === selectedName}>
              <button class="entity" onclick={() => select(e)}>
                <span class="ename">{e.name}</span>
                <span class="etype">{e.entityType}</span>
              </button>
              <button class="del" title="Delete entity" onclick={() => removeEntity(e.name)}>✕</button>
            </li>
          {/each}
        </ul>

        <div class="detail">
          {#if !selected}
            <p class="muted">Select an entity, or add one.</p>
          {:else}
            {#if editingEntity}
              <div class="entedit">
                <input class="entname" placeholder="Name" bind:value={entName}
                  onkeydown={(e) => {
                    if (e.key === 'Enter') saveEntity();
                    else if (e.key === 'Escape') cancelEntityEdit();
                  }} />
                <input class="enttype" placeholder="Type" bind:value={entType}
                  onkeydown={(e) => {
                    if (e.key === 'Enter') saveEntity();
                    else if (e.key === 'Escape') cancelEntityEdit();
                  }} />
                <button class="act" title="Save" onclick={saveEntity} disabled={!entName.trim()}>✓</button>
                <button class="del" title="Cancel" onclick={cancelEntityEdit}>↩</button>
              </div>
            {:else}
              <div class="detailhead">
                <h3>{selected.name} <small>({selected.entityType})</small></h3>
                <button class="act" title="Rename / retype entity" onclick={startEntityEdit}>✎</button>
              </div>
            {/if}

            <div class="section-title">Observations</div>
            {#if selected.observations.length === 0}
              <p class="muted">(none)</p>
            {/if}
            {#each selected.observations as obs (obs)}
              <div class="row">
                {#if editingObs === obs}
                  <input class="editinput" bind:value={editText}
                    onkeydown={(e) => {
                      if (e.key === 'Enter') saveObs();
                      else if (e.key === 'Escape') cancelEdit();
                    }} />
                  <button class="act" title="Save" onclick={saveObs} disabled={!editText.trim()}>✓</button>
                  <button class="del" title="Cancel" onclick={cancelEdit}>↩</button>
                {:else}
                  <span class="rowtext">{obs}</span>
                  <button class="act" title="Edit observation" onclick={() => startEdit(obs)}>✎</button>
                  <button class="del" title="Delete observation"
                    onclick={() => run(() => deleteObservation(selected!.name, obs))}>✕</button>
                {/if}
              </div>
            {/each}
            <div class="addrow">
              <input placeholder="New observation…" bind:value={newObs}
                onkeydown={(e) => e.key === 'Enter' && addObs()} />
              <button onclick={addObs} disabled={!newObs.trim()}>Add</button>
            </div>

            <div class="section-title">Relations</div>
            {#if touching.length === 0}
              <p class="muted">(none)</p>
            {/if}
            {#each touching as r (relKey(r))}
              <div class="row">
                {#if editingRel && relKey(editingRel) === relKey(r)}
                  <span class="from">{r.from === selected.name ? selected.name + ' —…→' : '…→ ' + selected.name}</span>
                  <input class="reltype" placeholder="relation (e.g. owns)" bind:value={relEditType}
                    onkeydown={(e) => {
                      if (e.key === 'Enter') saveRel();
                      else if (e.key === 'Escape') cancelRelEdit();
                    }} />
                  <select bind:value={relEditTarget}>
                    {#each otherNames as n (n)}
                      <option value={n}>{n}</option>
                    {/each}
                  </select>
                  <button class="act" title="Save" onclick={saveRel}
                    disabled={!relEditType.trim() || !relEditTarget}>✓</button>
                  <button class="del" title="Cancel" onclick={cancelRelEdit}>↩</button>
                {:else}
                  <span class="rowtext">{relText(r)}</span>
                  <button class="act" title="Edit relation" onclick={() => startRelEdit(r)}>✎</button>
                  <button class="del" title="Delete relation" onclick={() => run(() => deleteRelation(r))}>✕</button>
                {/if}
              </div>
            {/each}
            <div class="addrow">
              <span class="from">{selected.name} —…→</span>
              <input class="reltype" placeholder="relation (e.g. owns)" bind:value={relType} />
              <select bind:value={relTarget} disabled={otherNames.length === 0}>
                {#each otherNames as n (n)}
                  <option value={n}>{n}</option>
                {/each}
              </select>
              <button onclick={addRel} disabled={!relType.trim() || !relTarget}>Add</button>
            </div>
          {/if}
        </div>
      </div>
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
    width: min(760px, 100%);
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

  .addbar {
    display: flex;
    gap: 0.5rem;
    padding: 0.75rem 1rem;
    border-bottom: 1px solid var(--border);
  }
  .addbar input { flex: 1 1 auto; min-width: 0; }

  .error {
    color: #e06c6c;
    padding: 0.5rem 1rem 0;
    margin: 0;
    font-size: 0.85rem;
  }

  .cols {
    display: grid;
    grid-template-columns: 240px 1fr;
    min-height: 0;
    flex: 1 1 auto;
    overflow: hidden;
  }
  .entities {
    list-style: none;
    margin: 0;
    padding: 0.5rem;
    overflow-y: auto;
    border-right: 1px solid var(--border);
  }
  .entities li {
    display: flex;
    align-items: center;
    gap: 0.25rem;
    border-radius: 4px;
  }
  .entities li.active { background: var(--panel); }
  .entity {
    flex: 1 1 auto;
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 0.1rem;
    background: transparent;
    border: 0;
    text-align: left;
    padding: 0.4rem 0.5rem;
    color: var(--text);
  }
  .entity:hover { background: transparent; }
  .ename { font-size: 0.9rem; }
  .etype { font-size: 0.72rem; color: var(--muted); }

  .detail {
    padding: 0.75rem 1rem;
    overflow-y: auto;
  }
  .detail h3 { margin: 0 0 0.5rem; font-size: 1rem; }
  .detail h3 small { color: var(--muted); font-weight: normal; }
  .detailhead {
    display: flex;
    align-items: center;
    gap: 0.25rem;
  }
  .detailhead h3 { flex: 1 1 auto; word-break: break-word; }
  .entedit {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.4rem;
    margin-bottom: 0.5rem;
  }
  .entname { flex: 1 1 8rem; min-width: 0; }
  .enttype { flex: 1 1 6rem; min-width: 0; }
  .section-title {
    margin-top: 0.85rem;
    margin-bottom: 0.35rem;
    font-size: 0.8rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--muted);
    border-top: 1px solid var(--border);
    padding-top: 0.6rem;
  }
  .muted { color: var(--muted); font-size: 0.85rem; margin: 0.25rem 0; }

  .row {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 0.5rem;
    padding: 0.2rem 0;
  }
  .rowtext { flex: 1 1 auto; font-size: 0.88rem; word-break: break-word; }
  .row .reltype { flex: 1 1 10rem; min-width: 0; }
  .row select { flex: 1 1 8rem; min-width: 0; }

  .addrow {
    display: flex;
    gap: 0.4rem;
    align-items: center;
    margin-top: 0.4rem;
    flex-wrap: wrap;
  }
  .addrow input { flex: 1 1 auto; min-width: 6rem; }
  .addrow .reltype { flex: 0 1 12rem; }
  .from { font-size: 0.82rem; color: var(--muted); white-space: nowrap; }

  .del {
    background: transparent;
    border: 0;
    color: var(--muted);
    padding: 0.1rem 0.4rem;
    font-size: 0.85rem;
  }
  .del:hover { color: #e06c6c; background: transparent; }
  .act {
    background: transparent;
    border: 0;
    color: var(--muted);
    padding: 0.1rem 0.4rem;
    font-size: 0.85rem;
  }
  .act:hover { color: var(--text); background: transparent; }
  .editinput { flex: 1 1 auto; min-width: 0; }
  .ghost { background: transparent; }

  input, select, button {
    color: var(--text);
    background: var(--panel);
    border: 1px solid var(--border);
    border-radius: 4px;
    padding: 0.4rem 0.5rem;
    font: inherit;
  }
  button { cursor: pointer; }
  button:disabled { opacity: 0.5; cursor: default; }
</style>
