// Earcons: the L2 rung of the audible-error ladder. Pure WebAudio
// oscillators, so they work with zero network and zero assets. Prebaked
// TTS phrases (L1) are fetched from the gateway and cached; if the fetch
// fails we fall back to the matching earcon.

let ctx: AudioContext | null = null;

/** Must be called from a user gesture (the connect tap) to unlock audio. */
export function unlockAudio(): void {
  if (!ctx) ctx = new AudioContext();
  if (ctx.state === "suspended") void ctx.resume();
}

function tone(freq: number, at: number, dur: number, type: OscillatorType = "sine", gain = 0.12): void {
  if (!ctx) return;
  const t0 = ctx.currentTime + at;
  const osc = ctx.createOscillator();
  const g = ctx.createGain();
  osc.type = type;
  osc.frequency.value = freq;
  g.gain.setValueAtTime(0, t0);
  g.gain.linearRampToValueAtTime(gain, t0 + 0.015);
  g.gain.linearRampToValueAtTime(0, t0 + dur);
  osc.connect(g).connect(ctx.destination);
  osc.start(t0);
  osc.stop(t0 + dur + 0.02);
}

export const earcons = {
  /** descending two-tone: link lost */
  linkLost(): void {
    tone(660, 0, 0.14);
    tone(440, 0.16, 0.2);
  },
  /** ascending two-tone: link restored */
  linkOk(): void {
    tone(440, 0, 0.12);
    tone(660, 0.14, 0.18);
  },
  /** triple blip: job done */
  jobDone(): void {
    tone(880, 0, 0.07);
    tone(880, 0.11, 0.07);
    tone(880, 0.22, 0.09);
  },
  /** low buzz: job failed */
  jobFail(): void {
    tone(150, 0, 0.35, "sawtooth", 0.08);
  },
  /** two mid blips: a job wants input */
  needsInput(): void {
    tone(560, 0, 0.09);
    tone(700, 0.13, 0.11);
  },
};

interface PrebakedAudio {
  element: HTMLAudioElement;
  objectURL: string;
}

interface PendingPrebakedAudio {
  controller: AbortController;
  promise: Promise<PrebakedAudio>;
}

const prebakedCache = new Map<string, PrebakedAudio>();
const pendingPrebaked = new Map<string, PendingPrebakedAudio>();
let prebakedGeneration = 0;
const prebakedFetchTimeoutMs = 10_000;

const earconFallback: Record<string, () => void> = {
  voice_lost: earcons.linkLost,
  voice_down: earcons.linkLost,
  backend_error: earcons.jobFail,
  resume_ok: earcons.linkOk,
  approval_needed: earcons.needsInput,
  budget_tripped: earcons.jobFail,
};

/** Load one prebaked phrase. Concurrent callers share one owned request. */
async function loadPrebaked(key: string): Promise<PrebakedAudio> {
  const cached = prebakedCache.get(key);
  if (cached) return cached;
  const pending = pendingPrebaked.get(key);
  if (pending) return pending.promise;

  const controller = new AbortController();
  const generation = prebakedGeneration;
  const promise = (async () => {
    const resp = await fetch(`/audio/prebaked/${key}.mp3`, {
      signal: AbortSignal.any([controller.signal, AbortSignal.timeout(prebakedFetchTimeoutMs)]),
    });
    if (!resp.ok) throw new Error(`http ${resp.status}`);
    const blob = await resp.blob();
    if (controller.signal.aborted || generation !== prebakedGeneration) {
      throw new Error("prebaked load disposed");
    }
    const objectURL = URL.createObjectURL(blob);
    try {
      const value = { element: new Audio(objectURL), objectURL };
      prebakedCache.set(key, value);
      return value;
    } catch (error) {
      URL.revokeObjectURL(objectURL);
      throw error;
    }
  })();
  const owned = { controller, promise };
  pendingPrebaked.set(key, owned);
  void promise.then(
    () => {
      if (pendingPrebaked.get(key) === owned) pendingPrebaked.delete(key);
    },
    () => {
      if (pendingPrebaked.get(key) === owned) pendingPrebaked.delete(key);
    },
  );
  return promise;
}

/** Play a prebaked TTS phrase by key; earcon fallback if unavailable. */
export async function playPrebaked(key: string): Promise<void> {
  try {
    const { element } = await loadPrebaked(key);
    element.currentTime = 0;
    await element.play();
  } catch (err) {
    console.debug("prebaked unavailable, earcon fallback", key, err);
    (earconFallback[key] ?? earcons.jobFail)();
  }
}

/** Warm the cache in the background after first gesture. */
export function prefetchPrebaked(keys: string[]): void {
  for (const key of new Set(keys)) {
    void loadPrebaked(key).catch(() => {});
  }
}

/** Abort owned requests and release every generated object URL. */
export function disposePrebaked(): void {
  prebakedGeneration += 1;
  for (const { controller } of pendingPrebaked.values()) controller.abort();
  pendingPrebaked.clear();
  for (const { objectURL } of prebakedCache.values()) URL.revokeObjectURL(objectURL);
  prebakedCache.clear();
}
