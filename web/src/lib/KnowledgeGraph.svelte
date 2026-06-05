<script lang="ts">
  // KnowledgeGraph.svelte renders the knowledge graph as a force-directed
  // node-link diagram: entities are bubbles (colored by type, sized by how
  // much is known about them), relations are labelled lines between them.
  // Clicking a bubble shows its observations and relations in a side panel;
  // bubbles can be dragged. The layout is a small dependency-free spring
  // simulation run on requestAnimationFrame, so there's no d3/npm footprint.

  import { onMount, onDestroy } from 'svelte';
  import {
    fetchGraph,
    addObservation,
    editObservation,
    deleteObservation,
    type KnowledgeGraph,
    type KGRelation,
  } from './hub';

  let { onback }: { onback: () => void } = $props();

  interface Node {
    name: string;
    type: string;
    obs: string[];
    x: number;
    y: number;
    vx: number;
    vy: number;
    r: number;
    fixed: boolean;
  }
  interface Link { s: number; t: number; rel: string }

  let nodes = $state<Node[]>([]);
  let links = $state<Link[]>([]);
  let loadError = $state('');
  let actionError = $state('');
  let selected = $state<number | null>(null);

  // Side-panel observation editing state. newObs is the add-row input;
  // editingObs holds the original text of the row being edited (its key),
  // editText the in-progress replacement.
  let newObs = $state('');
  let editingObs = $state('');
  let editText = $state('');

  let width = $state(800);
  let height = $state(600);
  let svgEl: SVGSVGElement;
  let raf = 0;
  let dragging: number | null = null;

  // Spring-simulation tuning. Tweaked for a few-dozen-node graph in a
  // ~800×600 viewport.
  const REPULSION = 7000;
  const LINK_LEN = 130;
  const SPRING = 0.02;
  const CENTER = 0.006;
  const DAMPING = 0.85;

  function colorFor(type: string): string {
    let h = 0;
    for (const c of type || 'thing') h = (h * 31 + c.charCodeAt(0)) % 360;
    return `hsl(${h} 55% 55%)`;
  }

  async function load() {
    try {
      const g: KnowledgeGraph = await fetchGraph();
      loadError = '';
      buildLayout(g);
    } catch (e) {
      loadError = (e as Error).message;
    }
  }

  function buildLayout(g: KnowledgeGraph) {
    const idx = new Map<string, number>();
    const ns: Node[] = g.entities.map((e, i) => {
      idx.set(e.name, i);
      // Seed positions on a circle so the sim untangles instead of starting
      // from a degenerate stack.
      const a = (i / Math.max(1, g.entities.length)) * Math.PI * 2;
      return {
        name: e.name,
        type: e.entityType,
        obs: e.observations ?? [],
        x: width / 2 + Math.cos(a) * 180,
        y: height / 2 + Math.sin(a) * 180,
        vx: 0,
        vy: 0,
        r: 16 + Math.min((e.observations ?? []).length, 6) * 2,
        fixed: false,
      };
    });
    const ls: Link[] = [];
    for (const r of g.relations) {
      const s = idx.get(r.from);
      const t = idx.get(r.to);
      if (s !== undefined && t !== undefined) ls.push({ s, t, rel: r.relationType });
    }
    nodes = ns;
    links = ls;
    selected = null;
    restart();
  }

  function tick() {
    const n = nodes;
    // Pairwise repulsion.
    for (let i = 0; i < n.length; i++) {
      for (let j = i + 1; j < n.length; j++) {
        let dx = n[j].x - n[i].x;
        let dy = n[j].y - n[i].y;
        let d2 = dx * dx + dy * dy || 0.01;
        let d = Math.sqrt(d2);
        let f = REPULSION / d2;
        let fx = (dx / d) * f;
        let fy = (dy / d) * f;
        n[i].vx -= fx; n[i].vy -= fy;
        n[j].vx += fx; n[j].vy += fy;
      }
    }
    // Link springs.
    for (const l of links) {
      const a = n[l.s], b = n[l.t];
      let dx = b.x - a.x, dy = b.y - a.y;
      let d = Math.sqrt(dx * dx + dy * dy) || 0.01;
      let f = (d - LINK_LEN) * SPRING;
      let fx = (dx / d) * f, fy = (dy / d) * f;
      a.vx += fx; a.vy += fy;
      b.vx -= fx; b.vy -= fy;
    }
    // Centering gravity + integrate.
    let energy = 0;
    for (const node of n) {
      node.vx += (width / 2 - node.x) * CENTER;
      node.vy += (height / 2 - node.y) * CENTER;
      node.vx *= DAMPING;
      node.vy *= DAMPING;
      if (!node.fixed) {
        node.x += node.vx;
        node.y += node.vy;
      }
      node.x = Math.max(node.r, Math.min(width - node.r, node.x));
      node.y = Math.max(node.r, Math.min(height - node.r, node.y));
      energy += node.vx * node.vx + node.vy * node.vy;
    }
    return energy;
  }

  function loop() {
    const energy = tick();
    // Settle: stop animating once it's basically still (and nothing is being
    // dragged), to spare the CPU. Any drag/reload restarts the loop.
    if (energy < 0.05 && dragging === null) {
      raf = 0;
      return;
    }
    raf = requestAnimationFrame(loop);
  }

  function restart() {
    if (raf === 0) raf = requestAnimationFrame(loop);
  }

  // ── Pointer drag ──
  function toLocal(e: PointerEvent): { x: number; y: number } {
    const rect = svgEl.getBoundingClientRect();
    return {
      x: ((e.clientX - rect.left) / rect.width) * width,
      y: ((e.clientY - rect.top) / rect.height) * height,
    };
  }
  function onPointerDown(i: number, e: PointerEvent) {
    e.preventDefault();
    dragging = i;
    nodes[i].fixed = true;
    // Switching to a different bubble clears any in-progress observation edit.
    if (selected !== i) resetObsForms();
    selected = i;
    (e.target as Element).setPointerCapture(e.pointerId);
    restart();
  }
  function onPointerMove(e: PointerEvent) {
    if (dragging === null) return;
    const p = toLocal(e);
    nodes[dragging].x = p.x;
    nodes[dragging].y = p.y;
    nodes[dragging].vx = 0;
    nodes[dragging].vy = 0;
    restart();
  }
  function onPointerUp() {
    if (dragging !== null) nodes[dragging].fixed = false;
    dragging = null;
    restart();
  }

  function onResize() {
    if (!svgEl) return;
    const rect = svgEl.getBoundingClientRect();
    width = Math.max(320, rect.width);
    height = Math.max(320, rect.height);
    restart();
  }

  onMount(() => {
    onResize();
    window.addEventListener('resize', onResize);
    load();
  });
  onDestroy(() => {
    if (raf) cancelAnimationFrame(raf);
    window.removeEventListener('resize', onResize);
  });

  const sel = $derived(selected !== null ? nodes[selected] : null);
  function relsOf(name: string): KGRelation[] {
    return links
      .filter(l => nodes[l.s].name === name || nodes[l.t].name === name)
      .map(l => ({ from: nodes[l.s].name, to: nodes[l.t].name, relationType: l.rel }));
  }

  // ── Observation editing ──
  // Mutations write straight through the REST API — the same graph the model
  // reads each turn — then patch the fresh observations back into the existing
  // nodes (by name) instead of calling load(): a full rebuild would re-seed the
  // layout and drop the selection, jarring mid-edit.
  async function refreshObs() {
    const g = await fetchGraph();
    const byName = new Map(g.entities.map(e => [e.name, e.observations ?? []]));
    for (const node of nodes) {
      const obs = byName.get(node.name);
      if (obs) {
        node.obs = obs;
        node.r = 16 + Math.min(obs.length, 6) * 2;
      }
    }
    restart();
  }

  async function run(fn: () => Promise<void>) {
    actionError = '';
    try {
      await fn();
      await refreshObs();
    } catch (e) {
      actionError = (e as Error).message;
    }
  }

  function resetObsForms() {
    newObs = '';
    editingObs = '';
    editText = '';
    actionError = '';
  }

  async function addObs() {
    const content = newObs.trim();
    if (!content || !sel) return;
    const entity = sel.name;
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

  // saveObs rewrites the edited observation in place; unchanged or empty text
  // just closes the editor.
  async function saveObs() {
    const orig = editingObs;
    const next = editText.trim();
    if (!orig || !sel) return;
    if (!next || next === orig) {
      cancelEdit();
      return;
    }
    const entity = sel.name;
    await run(() => editObservation(entity, orig, next));
    cancelEdit();
  }

  async function removeObs(obs: string) {
    if (!sel) return;
    const entity = sel.name;
    await run(() => deleteObservation(entity, obs));
  }
</script>

<div class="page">
  <header>
    <button class="back" onclick={onback}>← Chat</button>
    <h2>Knowledge graph</h2>
    <div class="spacer"></div>
    <button onclick={load} title="Reload">⟳ Reload</button>
  </header>

  {#if loadError}
    <p class="error">{loadError}</p>
  {:else if nodes.length === 0}
    <p class="empty">No entities yet — Diesel will fill this in as he learns about you.</p>
  {/if}

  <div class="stage">
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <svg
      bind:this={svgEl}
      viewBox="0 0 {width} {height}"
      onpointermove={onPointerMove}
      onpointerup={onPointerUp}
      onpointerleave={onPointerUp}
    >
      {#each links as l (l.s + '-' + l.t + '-' + l.rel)}
        {@const a = nodes[l.s]}
        {@const b = nodes[l.t]}
        <line x1={a.x} y1={a.y} x2={b.x} y2={b.y} class="edge" />
        <text x={(a.x + b.x) / 2} y={(a.y + b.y) / 2 - 3} class="edgelabel">{l.rel}</text>
      {/each}
      {#each nodes as n, i (n.name)}
        <!-- svelte-ignore a11y_click_events_have_key_events -->
        <g
          class="node"
          class:selected={selected === i}
          onpointerdown={(e) => onPointerDown(i, e)}
        >
          <circle cx={n.x} cy={n.y} r={n.r} fill={colorFor(n.type)} />
          <text x={n.x} y={n.y + n.r + 13} class="nodelabel">{n.name}</text>
        </g>
      {/each}
    </svg>

    {#if sel}
      <aside class="panel">
        <h3>{sel.name}</h3>
        <div class="type" style="color:{colorFor(sel.type)}">{sel.type}</div>
        <div class="sec">Observations</div>
        {#if sel.obs.length === 0}<p class="muted">(none)</p>{/if}
        {#each sel.obs as o (o)}
          <div class="orow">
            {#if editingObs === o}
              <input class="oedit" bind:value={editText}
                onkeydown={(e) => {
                  if (e.key === 'Enter') saveObs();
                  else if (e.key === 'Escape') cancelEdit();
                }} />
              <button class="act" title="Save" onclick={saveObs} disabled={!editText.trim()}>✓</button>
              <button class="act" title="Cancel" onclick={cancelEdit}>↩</button>
            {:else}
              <span class="otext">{o}</span>
              <button class="act" title="Edit observation" onclick={() => startEdit(o)}>✎</button>
              <button class="act del" title="Delete observation" onclick={() => removeObs(o)}>✕</button>
            {/if}
          </div>
        {/each}
        <div class="oadd">
          <input placeholder="New observation…" bind:value={newObs}
            onkeydown={(e) => e.key === 'Enter' && addObs()} />
          <button onclick={addObs} disabled={!newObs.trim()}>Add</button>
        </div>
        {#if actionError}<p class="error panelerror">{actionError}</p>{/if}

        <div class="sec">Relations</div>
        {#each relsOf(sel.name) as r (r.from + r.relationType + r.to)}
          <div class="rel">{r.from} —{r.relationType}→ {r.to}</div>
        {/each}
      </aside>
    {/if}
  </div>
</div>

<style>
  .page {
    display: flex;
    flex-direction: column;
    height: 100vh;
  }
  header {
    flex: 0 0 auto;
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding: 0.65rem 1rem;
    border-bottom: 1px solid var(--border);
  }
  header h2 { margin: 0; font-size: 1.05rem; }
  .spacer { flex: 1 1 auto; }
  .back { background: transparent; }

  .error { color: #e06c6c; padding: 0.5rem 1rem; }
  .empty { color: var(--muted); padding: 0.5rem 1rem; }

  .stage {
    position: relative;
    flex: 1 1 auto;
    min-height: 0;
  }
  svg {
    width: 100%;
    height: 100%;
    display: block;
    touch-action: none;
    background:
      radial-gradient(circle at 50% 40%, rgba(255, 255, 255, 0.03), transparent 70%);
  }
  .edge { stroke: var(--border); stroke-width: 1.5; }
  .edgelabel {
    fill: var(--muted);
    font-size: 10px;
    text-anchor: middle;
    pointer-events: none;
  }
  .node { cursor: grab; }
  .node:active { cursor: grabbing; }
  .node circle {
    stroke: rgba(0, 0, 0, 0.35);
    stroke-width: 1.5;
  }
  .node.selected circle {
    stroke: var(--text);
    stroke-width: 2.5;
  }
  .nodelabel {
    fill: var(--text);
    font-size: 11px;
    text-anchor: middle;
    pointer-events: none;
  }

  .panel {
    position: absolute;
    top: 0.75rem;
    right: 0.75rem;
    width: 260px;
    max-height: calc(100% - 1.5rem);
    overflow-y: auto;
    background: var(--panel);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.75rem 0.9rem;
    box-shadow: 0 8px 30px rgba(0, 0, 0, 0.35);
  }
  .panel h3 { margin: 0; font-size: 1rem; }
  .panel .type { font-size: 0.8rem; margin-bottom: 0.4rem; }
  .sec {
    margin-top: 0.7rem;
    font-size: 0.72rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--muted);
    border-top: 1px solid var(--border);
    padding-top: 0.5rem;
  }
  .rel { font-size: 0.82rem; margin: 0.15rem 0; word-break: break-word; }
  .muted { color: var(--muted); font-size: 0.85rem; margin: 0.2rem 0; }

  .orow {
    display: flex;
    align-items: center;
    gap: 0.25rem;
    margin: 0.15rem 0;
  }
  .otext { flex: 1 1 auto; font-size: 0.85rem; word-break: break-word; }
  .oedit { flex: 1 1 auto; min-width: 0; padding: 0.25rem 0.4rem; font-size: 0.85rem; }
  .oadd {
    display: flex;
    gap: 0.3rem;
    margin-top: 0.4rem;
  }
  .oadd input { flex: 1 1 auto; min-width: 0; padding: 0.3rem 0.4rem; font-size: 0.85rem; }
  .oadd button { padding: 0.3rem 0.6rem; font-size: 0.82rem; }
  .act {
    flex: 0 0 auto;
    background: transparent;
    border: 0;
    color: var(--muted);
    padding: 0.05rem 0.3rem;
    font-size: 0.82rem;
    line-height: 1;
    cursor: pointer;
  }
  .act:hover:not(:disabled) { background: transparent; color: var(--text); }
  .act.del:hover:not(:disabled) { color: #e06c6c; }
  .panelerror { padding: 0.4rem 0 0; font-size: 0.8rem; }
</style>
