// Minimal writable store implementation — a near-drop-in for
// svelte/store's Writable, kept local so the only Svelte runtime
// dependency is the framework itself. Using $effect / $state in
// components would also work, but a writable lets non-component
// modules (hub.ts) own state and notify components reactively.

export interface Writable<T> {
  set(value: T): void;
  update(fn: (cur: T) => T): void;
  subscribe(run: (value: T) => void): () => void;
}

export function writable<T>(initial: T): Writable<T> {
  let value = initial;
  const subs = new Set<(v: T) => void>();
  return {
    set(v) {
      if (v === value) return;
      value = v;
      subs.forEach(s => s(value));
    },
    update(fn) {
      this.set(fn(value));
    },
    subscribe(run) {
      subs.add(run);
      run(value);
      return () => subs.delete(run);
    },
  };
}

// get reads the current value of a store without subscribing. Match
// svelte/store's helper of the same name so calling code is familiar.
export function get<T>(store: Writable<T>): T {
  let current!: T;
  const unsub = store.subscribe(v => { current = v; });
  unsub();
  return current;
}
