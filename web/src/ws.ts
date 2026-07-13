// Control link to the gateway: bus events in, client state and text out.
// Auto-reconnects with backoff; a heartbeat watchdog detects a dead
// backend (the one failure nobody server-side can announce) and lets the
// app sound the L2 earcon.

export interface BusEvent {
  seq: number;
  ts: string;
  kind: string;
  job_id?: string;
  payload?: unknown;
}

type ServerMsg =
  | { type: "hb" }
  | { type: "event"; event: BusEvent };

export interface LinkCallbacks {
  onEvent: (e: BusEvent) => void;
  onLink: (up: boolean) => void;
}

export class ControlLink {
  private ws: WebSocket | null = null;
  private cb: LinkCallbacks;
  private lastBeat = 0;
  private up = false;
  private backoff = 1000;
  private closed = false;
  private watchdog: number | undefined;

  constructor(cb: LinkCallbacks) {
    this.cb = cb;
  }

  start(): void {
    this.closed = false;
    this.connect();
    this.watchdog = window.setInterval(() => this.checkPulse(), 3000);
  }

  stop(): void {
    this.closed = true;
    if (this.watchdog) window.clearInterval(this.watchdog);
    this.ws?.close();
  }

  private wsURL(): string {
    const proto = location.protocol === "https:" ? "wss" : "ws";
    return `${proto}://${location.host}/ws`;
  }

  private connect(): void {
    if (this.closed) return;
    const ws = new WebSocket(this.wsURL());
    this.ws = ws;
    ws.onopen = () => {
      console.debug("control ws open");
      this.backoff = 1000;
      this.lastBeat = Date.now();
      this.setUp(true);
    };
    ws.onmessage = (ev: MessageEvent<string>) => {
      this.lastBeat = Date.now();
      let msg: ServerMsg;
      try {
        msg = JSON.parse(ev.data) as ServerMsg;
      } catch {
        return;
      }
      if (msg.type === "event") this.cb.onEvent(msg.event);
    };
    ws.onclose = () => {
      this.setUp(false);
      if (!this.closed) {
        window.setTimeout(() => this.connect(), this.backoff);
        this.backoff = Math.min(this.backoff * 2, 8000);
      }
    };
    ws.onerror = () => ws.close();
  }

  private checkPulse(): void {
    // Server heartbeats every 5s; three misses = link down.
    if (this.up && Date.now() - this.lastBeat > 16000) this.setUp(false);
  }

  private setUp(up: boolean): void {
    if (this.up === up) return;
    this.up = up;
    this.cb.onLink(up);
  }

  send(obj: Record<string, unknown>): boolean {
    if (this.ws?.readyState !== WebSocket.OPEN) return false;
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
