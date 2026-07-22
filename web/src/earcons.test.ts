import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { disposePrebaked, playPrebaked, prefetchPrebaked } from "./earcons";

interface FakeAudioElement {
  src: string;
  currentTime: number;
  play: ReturnType<typeof vi.fn>;
}

let audioElements: FakeAudioElement[];
let createObjectURL: ReturnType<typeof vi.spyOn>;
let revokeObjectURL: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  audioElements = [];
  class FakeAudio {
    currentTime = 0;
    play = vi.fn(async () => {});

    constructor(readonly src: string) {
      audioElements.push(this);
    }
  }
  vi.stubGlobal("Audio", FakeAudio);
  createObjectURL = vi.spyOn(URL, "createObjectURL").mockReturnValueOnce("blob:first").mockReturnValue("blob:next");
  revokeObjectURL = vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
});

afterEach(() => {
  disposePrebaked();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("prebaked audio ownership", () => {
  it("deduplicates concurrent loads and reuses the owned object URL", async () => {
    const fetchMock = vi.fn(async () => new Response(new Blob(["audio"]), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    await Promise.all([playPrebaked("voice_down"), playPrebaked("voice_down")]);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(createObjectURL).toHaveBeenCalledTimes(1);
    expect(audioElements).toHaveLength(1);
    expect(audioElements[0].play).toHaveBeenCalledTimes(2);
  });

  it("revokes cached URLs on disposal and reloads after a new owner starts", async () => {
    const fetchMock = vi.fn(async () => new Response(new Blob(["audio"]), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    await playPrebaked("resume_ok");
    disposePrebaked();
    expect(revokeObjectURL).toHaveBeenCalledWith("blob:first");

    await playPrebaked("resume_ok");
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(createObjectURL).toHaveBeenCalledTimes(2);
  });

  it("aborts in-flight prefetches and cannot populate the cache after disposal", async () => {
    let requestSignal: AbortSignal | undefined;
    const fetchMock = vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
      requestSignal = init?.signal ?? undefined;
      return new Promise<Response>((_resolve, reject) => {
        requestSignal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true });
      });
    });
    vi.stubGlobal("fetch", fetchMock);

    prefetchPrebaked(["voice_lost", "voice_lost"]);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    disposePrebaked();

    await vi.waitFor(() => expect(requestSignal?.aborted).toBe(true));
    expect(createObjectURL).not.toHaveBeenCalled();
  });
});
