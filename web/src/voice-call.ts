export function waitForIce(pc: RTCPeerConnection, timeoutMs: number, signal: AbortSignal): Promise<void> {
  if (signal.aborted || pc.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    const finish = () => {
      globalThis.clearTimeout(timer);
      pc.removeEventListener("icegatheringstatechange", onChange);
      signal.removeEventListener("abort", finish);
      resolve();
    };
    const onChange = () => {
      if (pc.iceGatheringState === "complete") finish();
    };
    const timer = globalThis.setTimeout(finish, timeoutMs);
    pc.addEventListener("icegatheringstatechange", onChange);
    signal.addEventListener("abort", finish, { once: true });
  });
}

export async function waitForRemoteAudioDrain(
  pc: RTCPeerConnection,
  maxWaitMs: number,
  quietWindowMs: number,
  signal: AbortSignal,
): Promise<void> {
  const started = performance.now();
  let quietSince = started;
  while (!signal.aborted && performance.now() - started < maxWaitMs) {
    if (pc.connectionState === "closed" || pc.connectionState === "failed") return;
    const now = performance.now();
    let level = 0;
    for (const receiver of pc.getReceivers()) {
      if (receiver.track?.kind !== "audio") continue;
      for (const source of receiver.getSynchronizationSources()) {
        // Ignore stale RTP observations. A stream that has stopped sending is
        // quiet once its last packet has aged out of the receiver window.
        if (now - source.timestamp <= 500) level = Math.max(level, source.audioLevel ?? 0);
      }
    }
    if (level <= 0.01) {
      if (now - quietSince >= quietWindowMs) return;
    } else {
      quietSince = now;
    }
    await abortableDelay(50, signal);
  }
}

export function abortableDelay(delayMs: number, signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.resolve();
  return new Promise((resolve) => {
    const finish = () => {
      globalThis.clearTimeout(timer);
      signal.removeEventListener("abort", finish);
      resolve();
    };
    const timer = globalThis.setTimeout(finish, delayMs);
    signal.addEventListener("abort", finish, { once: true });
  });
}
