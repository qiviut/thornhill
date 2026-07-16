import { describe, expect, it } from "vitest";
import { ControlLink, type ControlLinkRuntime } from "./ws";

class FakeSocket {
  readyState = 0;
  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onclose: ((event: CloseEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  sent: string[] = [];

  open(): void {
    this.readyState = 1;
    this.onopen?.(new Event("open"));
  }

  message(data: string): void {
    this.onmessage?.({ data } as MessageEvent<string>);
  }

  close(): void {
    if (this.readyState === 3) return;
    this.readyState = 3;
    this.onclose?.(new Event("close") as CloseEvent);
  }

  send(data: string): void {
    this.sent.push(data);
  }
}

interface Timer {
  at: number;
  every: number;
  fn: () => void;
}

class FakeRuntime implements ControlLinkRuntime {
  protocol = "https:";
  host = "thornhill.test";
  sockets: FakeSocket[] = [];
  private clock = 0;
  private nextTimer = 1;
  private timers = new Map<number, Timer>();

  createSocket = (_url: string): WebSocket => {
    const socket = new FakeSocket();
    this.sockets.push(socket);
    return socket as unknown as WebSocket;
  };

  now = (): number => this.clock;

  setInterval = (fn: () => void, delay: number): number => this.addTimer(fn, delay, delay);
  clearInterval = (id: number): void => {
    this.timers.delete(id);
  };
  setTimeout = (fn: () => void, delay: number): number => this.addTimer(fn, delay, 0);
  clearTimeout = (id: number): void => {
    this.timers.delete(id);
  };

  advance(ms: number): void {
    const target = this.clock + ms;
    while (true) {
      const due = [...this.timers.entries()]
        .filter(([, timer]) => timer.at <= target)
        .sort((a, b) => a[1].at - b[1].at || a[0] - b[0])[0];
      if (!due) break;
      const [id, timer] = due;
      this.clock = timer.at;
      if (timer.every > 0) timer.at += timer.every;
      else this.timers.delete(id);
      timer.fn();
    }
    this.clock = target;
  }

  private addTimer(fn: () => void, delay: number, every: number): number {
    const id = this.nextTimer;
    this.nextTimer += 1;
    this.timers.set(id, { at: this.clock + delay, every, fn });
    return id;
  }
}

describe("ControlLink", () => {
  it("closes a half-open socket and reconnects exactly once", () => {
    const runtime = new FakeRuntime();
    const linkStates: boolean[] = [];
    const link = new ControlLink({ onEvent: () => {}, onLink: (up) => linkStates.push(up) }, runtime);

    link.start();
    expect(runtime.sockets).toHaveLength(1);
    runtime.sockets[0].open();
    expect(linkStates).toEqual([true]);
	const staleClose = runtime.sockets[0].onclose;

    runtime.advance(18_001);
    expect(runtime.sockets[0].readyState).toBe(3);
    expect(linkStates).toEqual([true, false]);
    expect(runtime.sockets).toHaveLength(2);
	staleClose?.(new Event("close") as CloseEvent);
	runtime.advance(20_000);
	expect(runtime.sockets).toHaveLength(2);
    runtime.sockets[1].open();
    expect(linkStates).toEqual([true, false, true]);
  });

  it("owns and cancels reconnect timers on stop", () => {
    const runtime = new FakeRuntime();
    const link = new ControlLink({ onEvent: () => {}, onLink: () => {} }, runtime);

    link.start();
    runtime.sockets[0].close();
    link.stop();
    runtime.advance(20_000);
    expect(runtime.sockets).toHaveLength(1);
  });

  it("reports malformed frames without dispatching them", () => {
    const runtime = new FakeRuntime();
    const events: string[] = [];
    const errors: string[] = [];
    const link = new ControlLink(
      {
        onEvent: (event) => events.push(event.kind),
        onLink: () => {},
        onProtocolError: (message) => errors.push(message),
      },
      runtime,
    );

    link.start();
    runtime.sockets[0].open();
    runtime.sockets[0].message('{"type":"event","event":{"kind":"job.done"}}');
    runtime.sockets[0].message(
		'{"type":"event","event":{"seq":1,"ts":"2026-07-16T00:00:00Z","kind":"job.done","job_id":"j1","payload":{"id":"j1","display_name":"One","status":"done"}}}',
    );

    expect(errors).toHaveLength(1);
    expect(events).toEqual(["job.done"]);
  });
});
