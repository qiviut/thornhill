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

const prebakedCache = new Map<string, HTMLAudioElement>();

const earconFallback: Record<string, () => void> = {
  voice_lost: earcons.linkLost,
  voice_down: earcons.linkLost,
  backend_error: earcons.jobFail,
  resume_ok: earcons.linkOk,
  approval_needed: earcons.needsInput,
  budget_tripped: earcons.jobFail,
};

/** Play a prebaked TTS phrase by key; earcon fallback if unavailable. */
export async function playPrebaked(key: string): Promise<void> {
  try {
    let el = prebakedCache.get(key);
    if (!el) {
      const resp = await fetch(`/audio/prebaked/${key}.mp3`);
      if (!resp.ok) throw new Error(`http ${resp.status}`);
      const blob = await resp.blob();
      el = new Audio(URL.createObjectURL(blob));
      prebakedCache.set(key, el);
    }
    el.currentTime = 0;
    await el.play();
  } catch (err) {
    console.debug("prebaked unavailable, earcon fallback", key, err);
    (earconFallback[key] ?? earcons.jobFail)();
  }
}

/** Warm the cache in the background after first gesture. */
export function prefetchPrebaked(keys: string[]): void {
  for (const key of keys) {
    if (prebakedCache.has(key)) continue;
    void fetch(`/audio/prebaked/${key}.mp3`)
      .then(async (r) => {
        if (!r.ok) return;
        const blob = await r.blob();
        prebakedCache.set(key, new Audio(URL.createObjectURL(blob)));
      })
      .catch(() => {});
  }
}
