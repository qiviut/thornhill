// Control link to the gateway: bus events in, client state and text out.
// Auto-reconnects with backoff; a heartbeat watchdog detects a dead
// backend (the one failure nobody server-side can announce) and lets the
// app sound the L2 earcon.

import { parseServerMessage, type BusEvent } from "./protocol";

export type { BusEvent } from "./protocol";

export interface LinkCallbacks {
  onEvent: (e: BusEvent) => void;
  onLink: (up: boolean) => void;
  onProtocolError?: (message: string) => void;
}

export interface ControlLinkRuntime {
  createSocket: (url: string) => WebSocket;
  now: () => number;
  protocol: string;
  host: string;
  setInterval: (fn: () => void, delay: number) => number;
  clearInterval: (id: number) => void;
  setTimeout: (fn: () => void, delay: number) => number;
  clearTimeout: (id: number) => void;
}

function createBrowserRuntime(): ControlLinkRuntime {
  return {
    createSocket: (url) => new WebSocket(url),
    now: () => Date.now(),
    protocol: location.protocol,
    host: location.host,
    setInterval: (fn, delay) => window.setInterval(fn, delay),
    clearInterval: (id) => window.clearInterval(id),
    setTimeout: (fn, delay) => window.setTimeout(fn, delay),
    clearTimeout: (id) => window.clearTimeout(id),
  };
}

export class ControlLink {
  private ws: WebSocket | null = null;
  private readonly cb: LinkCallbacks;
  private readonly runtime: ControlLinkRuntime;
  private lastBeat = 0;
  private up = false;
  private backoff = 1000;
  private closed = true;
  private watchdog: number | undefined;
  private reconnect: number | undefined;

  constructor(cb: LinkCallbacks, runtime?: ControlLinkRuntime) {
    this.cb = cb;
    this.runtime = runtime ?? createBrowserRuntime();
  }

  start(): void {
    if (!this.closed) return;
    this.closed = false;
    this.connect();
    this.watchdog = this.runtime.setInterval(() => this.checkPulse(), 3000);
  }

  stop(): void {
    this.closed = true;
    if (this.watchdog !== undefined) {
      this.runtime.clearInterval(this.watchdog);
      this.watchdog = undefined;
    }
    if (this.reconnect !== undefined) {
      this.runtime.clearTimeout(this.reconnect);
      this.reconnect = undefined;
    }
    const ws = this.ws;
    this.ws = null;
    ws?.close();
    this.setUp(false);
  }

  private wsURL(): string {
    const proto = this.runtime.protocol === "https:" ? "wss" : "ws";
    return `${proto}://${this.runtime.host}/ws`;
  }

  private connect(): void {
    if (this.closed) return;
    this.reconnect = undefined;
    const ws = this.runtime.createSocket(this.wsURL());
    this.ws = ws;
    ws.onopen = () => {
      if (this.closed || this.ws !== ws) return;
      console.debug("control ws open");
      this.backoff = 1000;
      this.lastBeat = this.runtime.now();
      this.setUp(true);
    };
    ws.onmessage = (ev: MessageEvent<string>) => {
      if (this.closed || this.ws !== ws) return;
      const msg = parseServerMessage(ev.data);
      if (!msg) {
        this.cb.onProtocolError?.("Ignored a malformed control-link message.");
        return;
      }
      this.lastBeat = this.runtime.now();
      if (msg.type === "event") this.cb.onEvent(msg.event);
    };
    ws.onclose = () => {
      if (this.ws !== ws) return;
      this.ws = null;
      this.setUp(false);
      this.scheduleReconnect();
    };
    ws.onerror = () => ws.close();
  }

  private scheduleReconnect(): void {
    if (this.closed || this.reconnect !== undefined) return;
    const delay = this.backoff;
    this.backoff = Math.min(this.backoff * 2, 8000);
    this.reconnect = this.runtime.setTimeout(() => this.connect(), delay);
  }

  private checkPulse(): void {
    // Server heartbeats every 5s; three misses means the socket is half-open.
    // Browser close events are not reliable in this state, so explicitly
    // transfer ownership to one replacement before closing the stale socket.
    if (!this.up || this.runtime.now() - this.lastBeat <= 16000) return;
    const stale = this.ws;
    this.ws = null;
    this.setUp(false);
    if (stale) {
      stale.onopen = null;
      stale.onmessage = null;
      stale.onerror = null;
      stale.onclose = null;
      stale.close();
    }
    this.connect();
  }

  private setUp(up: boolean): void {
    if (this.up === up) return;
    this.up = up;
    this.cb.onLink(up);
  }

  send(obj: Record<string, unknown>): boolean {
    if (this.ws?.readyState !== 1) return false;
    this.ws.send(JSON.stringify(obj));
    return true;
  }

  sendState(muted: boolean, hidden: boolean): boolean {
    return this.send({ type: "state", muted, hidden });
  }

  sendText(text: string): boolean {
    return this.send({ type: "text", text });
  }

  sendPark(): boolean {
    return this.send({ type: "park" });
  }
}
