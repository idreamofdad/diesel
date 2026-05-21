// hub.ts is the client-side mirror of the Go hub. It owns the WebSocket
// connection, the reactive state stores, and the helpers that send
// commands or fetch media. Components import the stores directly and
// call sendMessage / fetchAudio to interact.
//
// Reconnect: the WS reconnects with exponential backoff (1s → 30s)
// whenever it drops. Reconnects reuse the same client_id so the hub
// recognizes the returning subscriber and the last-active TTS routing
// keeps working across hiccups.

import { writable, get, type Writable } from './store';

// Mirrors hub.EventType on the Go side. Keep in sync with
// internal/hub/hub.go — the WS frames are JSON-marshalled hub.Event
// structs straight from the wire.
export type EventType =
  | 'turn_started'
  | 'turn_complete'
  | 'audio_ready'
  | 'portrait_ready'
  | 'portrait_progress'
  | 'turn_error'
  | 'status'
  | 'cleared'
  | 'busy'
  | 'ack';

export interface Message {
  role: 'user' | 'assistant' | 'system';
  content: string;
  timestamp?: string;
}

export interface Usage {
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
}

export interface HubEvent {
  type: EventType;
  origin?: string;
  user?: Message;
  assistant?: Message;
  emotion?: string;
  naked?: boolean;
  portrait_url?: string;
  audio_url?: string;
  usage?: Usage;
  status?: string;
  error?: string;
  step?: number;
  total?: number;
  timestamp?: string;
  // ack-only fields
  client_id?: string;
  in_flight?: boolean;
}

// clientID persists across reloads via localStorage so a refresh keeps
// the same hub identity (which keeps TTS routing consistent — last
// active is still "us" after F5).
const CID_KEY = 'diesel:client_id';
function getClientID(): string {
  let id = localStorage.getItem(CID_KEY);
  if (!id) {
    id = `web-${Math.random().toString(36).slice(2, 10)}-${Date.now()}`;
    localStorage.setItem(CID_KEY, id);
  }
  return id;
}

// Bearer token. Settable via the UI; persisted in localStorage so a
// reload doesn't lock us out. Lives in the URL on WS upgrade
// (?token=…) because the browser WebSocket constructor can't send
// custom headers.
const TOKEN_KEY = 'diesel:token';
export const authToken: Writable<string> = writable(localStorage.getItem(TOKEN_KEY) || '');
authToken.subscribe(v => localStorage.setItem(TOKEN_KEY, v || ''));

// Mute toggle for TTS playback in this browser. Defaults on; per-tab
// preference, not synced to other clients.
const MUTE_KEY = 'diesel:muted';
export const muted: Writable<boolean> = writable(localStorage.getItem(MUTE_KEY) === '1');
muted.subscribe(v => localStorage.setItem(MUTE_KEY, v ? '1' : '0'));

// ─── Reactive state stores ─────────────────────────────────────────────

export const history: Writable<Message[]> = writable([]);
export const statusText: Writable<string> = writable('Connecting…');
export const inFlight: Writable<boolean> = writable(false);
export const portraitURL: Writable<string> = writable('');
export const connected: Writable<boolean> = writable(false);
export const usage: Writable<Usage> = writable({});

// ─── Connection ────────────────────────────────────────────────────────

let ws: WebSocket | null = null;
let reconnectAttempt = 0;
let reconnectTimer: number | undefined;

function wsURL(): string {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const token = get(authToken);
  const tokenParam = token ? `&token=${encodeURIComponent(token)}` : '';
  return `${proto}://${location.host}/api/v1/ws?client_id=${encodeURIComponent(getClientID())}${tokenParam}`;
}

export async function connect(): Promise<void> {
  // Pull the initial state via REST before opening the socket so the
  // UI has something to paint immediately. The WS ack will refresh
  // status/in_flight; transcript history doesn't change between this
  // fetch and the next event because the hub serializes turns.
  try {
    const resp = await fetch('/api/v1/state', {
      headers: authHeaders(),
    });
    if (resp.ok) {
      const data = await resp.json();
      history.set(data.history || []);
      statusText.set(data.status || 'Ready');
      inFlight.set(!!data.in_flight);
      if (data.portrait_url) portraitURL.set(data.portrait_url);
    } else if (resp.status === 401) {
      statusText.set('✗ Unauthorized — set your token in Settings');
      return;
    }
  } catch {
    // Network down — let the WS path retry.
  }

  openSocket();
}

function openSocket() {
  if (ws) {
    try { ws.close(); } catch { /* ignore */ }
  }
  ws = new WebSocket(wsURL());
  ws.onopen = () => {
    connected.set(true);
    reconnectAttempt = 0;
  };
  ws.onmessage = (msg) => {
    let ev: HubEvent;
    try {
      ev = JSON.parse(msg.data);
    } catch {
      return;
    }
    handleEvent(ev);
  };
  ws.onclose = () => {
    connected.set(false);
    scheduleReconnect();
  };
  ws.onerror = () => {
    // onclose follows; let it drive the retry.
  };
}

function scheduleReconnect() {
  if (reconnectTimer !== undefined) return;
  // Exponential backoff capped at 30s.
  const delay = Math.min(30_000, 1_000 * 2 ** Math.min(reconnectAttempt, 5));
  reconnectAttempt++;
  reconnectTimer = window.setTimeout(() => {
    reconnectTimer = undefined;
    openSocket();
  }, delay);
}

function handleEvent(ev: HubEvent) {
  switch (ev.type) {
    case 'ack':
      if (ev.status) statusText.set(ev.status);
      if (typeof ev.in_flight === 'boolean') inFlight.set(ev.in_flight);
      if (ev.portrait_url) portraitURL.set(ev.portrait_url);
      break;
    case 'turn_started':
      inFlight.set(true);
      if (ev.user) history.update(h => [...h, ev.user!]);
      break;
    case 'turn_complete':
      // Text arrives independently of audio/portrait now — paint it
      // immediately and unlock input so the next turn can start.
      inFlight.set(false);
      if (ev.assistant) history.update(h => [...h, ev.assistant!]);
      if (ev.usage) usage.set(ev.usage);
      break;
    case 'audio_ready':
      // Last-active wins: only the originating client plays the reply.
      // Empty audio_url = "no audio for this turn" (TTS off / synth
      // failed) — quietly ignored; nothing more to do.
      if (ev.origin === getClientID() && ev.audio_url && !get(muted)) {
        playAudio(ev.audio_url);
      }
      break;
    case 'portrait_progress':
      // Intermediate preview frames stream during a render. Each frame
      // lives at its own URL, so no cache-bust is needed — and skipping
      // it keeps the URL stable for the brief window when the same
      // frame might re-arrive on a reconnect.
      if (ev.portrait_url) portraitURL.set(ev.portrait_url);
      break;
    case 'portrait_ready':
      if (ev.portrait_url) portraitURL.set(cacheBust(ev.portrait_url));
      break;
    case 'turn_error':
      inFlight.set(false);
      if (ev.error && ev.origin === getClientID()) {
        statusText.set('✗ ' + ev.error);
      }
      break;
    case 'status':
      if (ev.status) statusText.set(ev.status);
      break;
    case 'cleared':
      history.set([]);
      usage.set({});
      break;
    case 'busy':
      statusText.set('Busy — wait for the current reply to finish.');
      break;
  }
}

// cacheBust appends a timestamp query to the portrait URL so browser
// HTTP cache doesn't serve a stale image after a new one arrives with
// the same ID (rare, but happens on rapid back-to-back turns).
function cacheBust(url: string): string {
  return url + (url.includes('?') ? '&' : '?') + 't=' + Date.now();
}

function playAudio(url: string) {
  const token = get(authToken);
  // <audio> can't carry an Authorization header, so when a token is
  // configured we fetch the bytes and play from an ObjectURL instead.
  if (token) {
    fetch(url, { headers: authHeaders() })
      .then(r => r.blob())
      .then(blob => {
        const obj = URL.createObjectURL(blob);
        const a = new Audio(obj);
        a.onended = () => URL.revokeObjectURL(obj);
        a.play().catch(() => URL.revokeObjectURL(obj));
      })
      .catch(() => { /* silent */ });
  } else {
    const a = new Audio(url);
    a.play().catch(() => { /* silent — autoplay blocked, user can interact and retry */ });
  }
}

// ─── Commands ──────────────────────────────────────────────────────────

export function sendMessage(text: string) {
  text = text.trim();
  if (!text) return;
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: 'send', text }));
    return;
  }
  // WS down — fall back to REST so the user can still send while
  // reconnecting.
  fetch('/api/v1/send', {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ text, origin: getClientID() }),
  }).catch(() => { /* status row already shows ✗ from connect failure */ });
}

export async function uploadAudio(blob: Blob, filename: string) {
  const form = new FormData();
  form.append('file', blob, filename);
  form.append('origin', getClientID());
  const resp = await fetch('/api/v1/transcribe', {
    method: 'POST',
    headers: authHeaders(),
    body: form,
  });
  if (!resp.ok) {
    const err = await resp.text();
    throw new Error(err || `HTTP ${resp.status}`);
  }
  return resp.json() as Promise<{ text: string; sent?: boolean }>;
}

function authHeaders(): Record<string, string> {
  const t = get(authToken);
  return t ? { Authorization: `Bearer ${t}` } : {};
}

export function getClientId(): string {
  return getClientID();
}

// ─── Settings API ──────────────────────────────────────────────────────
// Thin wrappers over the /api/v1/settings routes. Mirrors the AppSettings
// struct on the Go side (internal/settings/settings.go) — keep field
// names in sync with the JSON tags there. Secrets come back as the
// sentinel "********"; sending the same sentinel back means "leave the
// stored value alone", which is how the form avoids ever having to
// know the real key.

export const SECRET_MASK = '********';

export interface AppSettings {
  theme: string;
  api_endpoint: string;
  api_key: string;
  model: string;
  system_prompt: string;
  history_messages: number;
  stt_endpoint: string;
  stt_api_key: string;
  stt_model: string;
  continuous_conversation: boolean;
  enable_tts: boolean;
  tts_endpoint: string;
  tts_api_key: string;
  tts_model: string;
  tts_voice: string;
  input_device: string;
  output_device: string;
  save_to_disk: boolean;
  enable_image_gen: boolean;
  comfyui_endpoint: string;
  image_prompt: string;
  image_clothing: string;
  image_nudity: string;
  image_negative_prompt: string;
  image_steps: number;
  enable_server: boolean;
  server_expose_network: boolean;
  server_port: number;
  server_auth_token: string;
}

export async function fetchSettings(): Promise<AppSettings> {
  const resp = await fetch('/api/v1/settings', { headers: authHeaders() });
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
  return resp.json();
}

export async function saveSettings(s: AppSettings): Promise<AppSettings> {
  const resp = await fetch('/api/v1/settings', {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(s),
  });
  if (!resp.ok) {
    const body = await resp.text();
    throw new Error(body || `HTTP ${resp.status}`);
  }
  return resp.json();
}

export interface ProbeBody {
  kind: 'llm' | 'stt' | 'tts' | 'image';
  endpoint: string;
  api_key?: string;
  model?: string;
  voice?: string;
  text?: string;
}

export async function probeModels(body: ProbeBody): Promise<{ models: string[]; context_length?: number; error?: string }> {
  const resp = await fetch('/api/v1/settings/models', {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
  return resp.json();
}

export async function testConnection(body: ProbeBody): Promise<string> {
  const resp = await fetch('/api/v1/settings/test', {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
  const data = await resp.json();
  return data.status || data.error || '';
}

// testTTS synthesizes a sample phrase and returns the audio blob (or
// throws with the server's error message). The caller is responsible
// for playing the blob and revoking its ObjectURL when done.
export async function testTTS(body: ProbeBody): Promise<Blob> {
  const resp = await fetch('/api/v1/settings/test-tts', {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
  const ct = resp.headers.get('Content-Type') || '';
  if (ct.startsWith('application/json')) {
    const data = await resp.json();
    throw new Error(data.error || 'TTS failed');
  }
  return resp.blob();
}
