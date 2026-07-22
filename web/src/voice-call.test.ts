import { afterEach, describe, expect, it, vi } from "vitest";
import { abortableDelay, waitForIce, waitForRemoteAudioDrain } from "./voice-call";

afterEach(() => vi.useRealTimers());

describe("voice call lifecycle helpers", () => {
  it("releases ICE listeners when gathering completes", async () => {
    let onChange: (() => void) | undefined;
    const pc = {
      iceGatheringState: "gathering",
      addEventListener: vi.fn((_event: string, callback: () => void) => {
        onChange = callback;
      }),
      removeEventListener: vi.fn(),
    };
    const controller = new AbortController();

    const pending = waitForIce(pc as unknown as RTCPeerConnection, 1_000, controller.signal);
    pc.iceGatheringState = "complete";
    onChange?.();
    await pending;

    expect(pc.removeEventListener).toHaveBeenCalledWith("icegatheringstatechange", onChange);
  });

  it("releases ICE listeners when call ownership is aborted", async () => {
    const pc = {
      iceGatheringState: "gathering",
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    };
    const controller = new AbortController();

    const pending = waitForIce(pc as unknown as RTCPeerConnection, 1_000, controller.signal);
    controller.abort();
    await pending;

    expect(pc.removeEventListener).toHaveBeenCalledTimes(1);
  });

  it("cancels an owned delay immediately", async () => {
    vi.useFakeTimers();
    const controller = new AbortController();
    const pending = abortableDelay(60_000, controller.signal);

    controller.abort();
    await pending;
    expect(vi.getTimerCount()).toBe(0);
  });

  it("does not wait for audio drain after a peer closes", async () => {
    const pc = {
      connectionState: "closed",
      getReceivers: vi.fn(() => []),
    };

    await expect(
      waitForRemoteAudioDrain(pc as unknown as RTCPeerConnection, 2_500, 300, new AbortController().signal),
    ).resolves.toBeUndefined();
    expect(pc.getReceivers).not.toHaveBeenCalled();
  });
});
