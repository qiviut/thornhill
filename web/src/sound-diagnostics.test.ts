import { afterEach, describe, expect, it, vi } from "vitest";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.resetModules();
});

function fakeContext(initialState: AudioContextState, resumeImpl?: () => Promise<void>) {
  const starts = vi.fn();
  const stop = vi.fn();
  const oscillator = {
    type: "sine",
    frequency: { value: 0 },
    connect: vi.fn(),
    start: starts,
    stop,
  };
  const gain = { gain: { setValueAtTime: vi.fn(), linearRampToValueAtTime: vi.fn() }, connect: vi.fn() };
  oscillator.connect.mockReturnValue(gain);
  gain.connect.mockReturnValue(gain);
  const context = {
    state: initialState,
    currentTime: 0,
    destination: {},
    createOscillator: vi.fn(() => oscillator),
    createGain: vi.fn(() => gain),
    resume: vi.fn(async () => {
      if (resumeImpl) await resumeImpl();
      context.state = "running";
    }),
  };
  // biome-ignore lint/complexity/useArrowFunction: AudioContext is constructed with new.
  vi.stubGlobal("AudioContext", vi.fn(function () { return context; }));
  return { context, starts };
}

describe("test sound", () => {
  it("schedules two notes on a running audio context", async () => {
    const { starts, context } = fakeContext("running");
    const { testSound } = await import("./earcons");
    await testSound();
    expect(context.resume).not.toHaveBeenCalled();
    expect(starts).toHaveBeenCalledTimes(2);
  });

  it("invokes resume immediately in the user gesture", async () => {
    const { starts, context } = fakeContext("suspended");
    const { testSound } = await import("./earcons");
    const result = testSound();
    expect(context.resume).toHaveBeenCalledTimes(1);
    await result;
    expect(starts).toHaveBeenCalledTimes(2);
  });

  it("reports a browser audio rejection instead of claiming playback", async () => {
    const { starts } = fakeContext("suspended", async () => { throw new Error("blocked"); });
    const { testSound } = await import("./earcons");
    await expect(testSound()).rejects.toThrow("blocked");
    expect(starts).not.toHaveBeenCalled();
  });
});
