import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ControlLink, type BusEvent } from "./ws";
import { earcons, playPrebaked, prefetchPrebaked, unlockAudio } from "./earcons";
import {
  type Job,
  parseJobSnapshot,
  type SessionState,
  upsertJobHistory,
} from "./protocol";
import {
  getPushStatus,
  type PushStatus,
  pushStatusLabel,
  subscribePush,
  unsubscribePush,
} from "./push";

interface TranscriptLine {
  id: string;
  who: "you" | "desk";
  text: string;
}

const PREBAKE_KEYS = ["voice_lost", "voice_down", "backend_error", "resume_ok", "approval_needed", "budget_tripped"];

export default function App() {
  const [session, setSession] = useState<SessionState>("PARKED");
  const [parkReason, setParkReason] = useState("");
  const [linkUp, setLinkUp] = useState(false);
  const [muted, setMuted] = useState(false);
  const [connecting, setConnecting] = useState(false);
  const connectingRef = useRef(false);
  const [jobs, setJobs] = useState<Map<string, Job>>(new Map());
  const [ticker, setTicker] = useState<TranscriptLine[]>([]);
  const [notice, setNotice] = useState("");
  const [text, setText] = useState("");
  const [pushStatus, setPushStatus] = useState<PushStatus>("checking");
  const [pushBusy, setPushBusy] = useState(false);

  const pcRef = useRef<RTCPeerConnection | null>(null);
  const micRef = useRef<MediaStream | null>(null);
  const audioRef = useRef<HTMLAudioElement | null>(null);
  const connectAbortRef = useRef<AbortController | null>(null);
  const drainAbortRef = useRef<AbortController | null>(null);
  const rolloverTimerRef = useRef<number | null>(null);
  const callGenerationRef = useRef(0);
  const linkRef = useRef<ControlLink | null>(null);
  const mutedRef = useRef(false);
  const sessionRef = useRef<SessionState>("PARKED");
  sessionRef.current = session;
  mutedRef.current = muted;

  const pushState = useCallback(() => {
    linkRef.current?.sendState(mutedRef.current, document.hidden);
  }, []);

  const cancelParkWork = useCallback(() => {
    drainAbortRef.current?.abort();
    drainAbortRef.current = null;
    if (rolloverTimerRef.current !== null) {
      window.clearTimeout(rolloverTimerRef.current);
      rolloverTimerRef.current = null;
    }
  }, []);

  const teardownCall = useCallback(() => {
    cancelParkWork();
    callGenerationRef.current += 1;
    connectAbortRef.current?.abort();
    connectAbortRef.current = null;
    connectingRef.current = false;
    setConnecting(false);
    pcRef.current?.close();
    pcRef.current = null;
    micRef.current?.getTracks().forEach((track) => {
      track.stop();
    });
    micRef.current = null;
    if (audioRef.current) audioRef.current.srcObject = null;
  }, [cancelParkWork]);

  const connect = useCallback(async () => {
    if (pcRef.current || connectingRef.current) return;
		cancelParkWork();
    const generation = callGenerationRef.current + 1;
    callGenerationRef.current = generation;
    const controller = new AbortController();
    connectAbortRef.current = controller;
    const ownsCall = () => callGenerationRef.current === generation && !controller.signal.aborted;
    connectingRef.current = true;
    setConnecting(true);
    setNotice("");
    unlockAudio();
    prefetchPrebaked(PREBAKE_KEYS);
    try {
      const mic = await navigator.mediaDevices.getUserMedia({ audio: true });
      if (!ownsCall()) {
        mic.getTracks().forEach((track) => {
          track.stop();
        });
        return;
      }
      micRef.current = mic;
      const pc = new RTCPeerConnection();
      pcRef.current = pc;
      for (const track of mic.getTracks()) {
        track.enabled = !mutedRef.current;
        pc.addTrack(track, mic);
      }
      pc.ontrack = (ev) => {
				if (ownsCall() && pcRef.current === pc && audioRef.current) audioRef.current.srcObject = ev.streams[0];
      };
      pc.onconnectionstatechange = () => {
				if (!ownsCall() || pcRef.current !== pc) return;
        console.debug("rtc state", pc.connectionState);
        if (pc.connectionState === "failed") {
          earcons.linkLost();
          teardownCall();
        }
      };

      const offer = await pc.createOffer();
      if (!ownsCall()) return;
      await pc.setLocalDescription(offer);
			if (!ownsCall()) return;
      await waitForIce(pc, 1500, controller.signal);
      if (!ownsCall()) return;

      const resp = await fetch("/offer", {
        method: "POST",
        headers: { "Content-Type": "application/sdp" },
        body: pc.localDescription?.sdp ?? offer.sdp ?? "",
        signal: AbortSignal.any([controller.signal, AbortSignal.timeout(15_000)]),
      });
      if (!resp.ok) {
        throw new Error(`offer relay: http ${resp.status}`);
      }
      const answer = await resp.text();
      if (!ownsCall()) return;
      await pc.setRemoteDescription({ type: "answer", sdp: answer });
			if (!ownsCall() || pcRef.current !== pc) return;
      pushState();
      console.debug("call up");
    } catch (err) {
      if (callGenerationRef.current !== generation) return;
      console.warn("connect failed", err);
      setNotice(String(err));
      teardownCall();
      void playPrebaked("voice_down");
    } finally {
      if (callGenerationRef.current === generation) {
        connectAbortRef.current = null;
        connectingRef.current = false;
        setConnecting(false);
      }
    }
  }, [cancelParkWork, pushState, teardownCall]);

  const finishPark = useCallback(async (reason: string) => {
    cancelParkWork();
    const parkedPC = pcRef.current;
    const generation = callGenerationRef.current;
    const drainController = new AbortController();
    drainAbortRef.current = drainController;
    if (parkedPC) {
      await waitForRemoteAudioDrain(parkedPC, 2500, 300, drainController.signal);
      // A replacement call or newer server state must never be torn down by a
      // stale park completion.
      if (
        drainController.signal.aborted ||
        callGenerationRef.current !== generation ||
        pcRef.current !== parkedPC ||
        sessionRef.current !== "PARKING"
      ) {
        return;
      }
    }
    if (drainController.signal.aborted || callGenerationRef.current !== generation || sessionRef.current !== "PARKING") return;
    drainAbortRef.current = null;
    sessionRef.current = "PARKED";
    setSession("PARKED");
    setParkReason(reason);
    teardownCall();
    if (reason === "rollover") {
      // Invisible re-materialization: same browser permission, new call.
      const rolloverGeneration = callGenerationRef.current;
      rolloverTimerRef.current = window.setTimeout(() => {
        rolloverTimerRef.current = null;
        if (callGenerationRef.current === rolloverGeneration && sessionRef.current === "PARKED") void connect();
      }, 250);
    }
  }, [cancelParkWork, connect, teardownCall]);

  const handleEvent = useCallback(
    (e: BusEvent) => {
      switch (e.kind) {
        case "session.state": {
          const { state: st, reason } = e.payload;
          if (st === "PARKED") {
            // The server has stopped producing audio, but RTP already in flight
            // can still be audible in the browser. Keep the visible drain state
            // and peer connection until the receiver reports a quiet window.
            sessionRef.current = "PARKING";
            setSession("PARKING");
            setParkReason(reason || "finishing current audio");
            void finishPark(reason);
            break;
          }
					cancelParkWork();
          sessionRef.current = st;
          setSession(st);
          setParkReason(reason);
          if (st === "PARKING") {
            // Server-originated park requests (including the voice tool) must
            // stop new input just like the Park button while remote audio drains.
            micRef.current?.getAudioTracks().forEach((track) => {
              track.enabled = false;
            });
          }
          break;
        }
        case "transcript.user":
          setTicker((t) => clip(t, { who: "you", text: e.payload.text }));
          break;
        case "transcript.assistant":
          setTicker((t) => clip(t, { who: "desk", text: e.payload.text }));
          break;
        case "job.queued":
        case "job.running":
        case "job.needs_input":
        case "job.needs_approval":
        case "job.approval_parked":
        case "job.approval_resolved":
        case "job.progress":
        case "job.done":
        case "job.failed":
        case "job.cancelled": {
          const job = parseJobSnapshot(e);
          if (!job) {
            setNotice(`Ignored a malformed ${e.kind} event.`);
            break;
          }
          // Job lifecycle events carry complete snapshots. Replace rather
          // than merge so omitted cleared fields cannot survive earlier state.
          setJobs((m) => upsertJobHistory(m, job));
          if (e.kind === "job.done") earcons.jobDone();
          if (e.kind === "job.failed") earcons.jobFail();
          if (e.kind === "job.needs_input") earcons.needsInput();
          if (e.kind === "job.needs_approval") {
            if (sessionRef.current === "PARKED") void playPrebaked("approval_needed");
            else earcons.needsInput();
          }
          break;
        }
        case "job.renamed": {
          const jobID = e.job_id;
          const { to } = e.payload;
          setJobs((m) => {
            const next = new Map(m);
            const prev = next.get(jobID);
            if (prev) next.set(jobID, { ...prev, display_name: to });
            return next;
          });
          break;
        }
        case "error.voice": {
          const msg = e.payload.message;
          setNotice(msg);
          const key = e.payload.play;
          if (key) void playPrebaked(key);
          break;
        }
				case "job.approval_auto_denied":
				case "job.approval_auto_allowed":
				case "hermes.hook":
				case "usage":
					break;
        default:
					assertNever(e);
      }
    },
    [cancelParkWork, finishPark],
  );

  useEffect(() => {
    const link = new ControlLink({
      onEvent: handleEvent,
      onLink: (up) => {
        setLinkUp(up);
        if (up) earcons.linkOk();
        else earcons.linkLost();
      },
      onProtocolError: setNotice,
    });
    linkRef.current = link;
    link.start();
    const vis = () => pushState();
    document.addEventListener("visibilitychange", vis);
    return () => {
      document.removeEventListener("visibilitychange", vis);
      link.stop();
      teardownCall();
    };
  }, [handleEvent, pushState, teardownCall]);

  useEffect(() => {
    const controller = new AbortController();
    void getPushStatus(controller.signal)
      .then(setPushStatus)
      .catch((error: unknown) => {
        if (!controller.signal.aborted) {
          console.warn("notification status failed", error);
          setPushStatus("unavailable");
        }
      });
    return () => controller.abort();
  }, []);

  const togglePush = useCallback(async () => {
    if (pushBusy) return;
    setPushBusy(true);
    setNotice("");
    try {
      setPushStatus(pushStatus === "subscribed" ? await unsubscribePush() : await subscribePush());
    } catch (error) {
      console.warn("notification enrollment failed", error);
      setNotice("Notification enrollment failed. Durable results remain available when you resume.");
    } finally {
      setPushBusy(false);
    }
  }, [pushBusy, pushStatus]);

  const toggleMute = useCallback(() => {
    setMuted((m) => {
      const next = !m;
      micRef.current?.getAudioTracks().forEach((track) => {
        track.enabled = !next;
      });
      mutedRef.current = next;
      linkRef.current?.sendState(next, document.hidden);
      return next;
    });
  }, []);

  const ringTap = useCallback(() => {
    if (sessionRef.current === "PARKED") void connect();
    else if (sessionRef.current !== "PARKING") toggleMute();
  }, [connect, toggleMute]);

  const park = useCallback(() => {
    if (!linkRef.current?.sendPark()) {
      setNotice("Control link is down; voice session was not parked.");
      return;
    }
    // Stop sending new microphone audio while keeping the peer connection and
    // remote audio alive until the server confirms the current output drained.
    micRef.current?.getAudioTracks().forEach((track) => {
      track.enabled = false;
    });
    sessionRef.current = "PARKING";
    setSession("PARKING");
    setParkReason("finishing current audio");
  }, []);

  const sendText = useCallback(() => {
    const trimmed = text.trim();
    if (!trimmed) return;
    linkRef.current?.sendText(trimmed);
    setTicker((t) => clip(t, { who: "you", text: trimmed }));
    setText("");
  }, [text]);

  const jobList = useMemo(() => {
    const arr = [...jobs.values()];
    const rank: Record<string, number> = { needs_approval: 0, parked_approval: 1, needs_input: 2, running: 3, queued: 4, failed: 5, done: 6, cancelled: 7 };
    arr.sort((a, b) => (rank[a.status] ?? 9) - (rank[b.status] ?? 9));
    return arr;
  }, [jobs]);

  const ringLabel =
    session === "PARKED"
      ? connecting
        ? "DIALING"
        : "TAP TO RESUME"
      : session === "PARKING"
        ? "DRAINING AUDIO"
        : muted
          ? "MUTED"
          : session;

  return (
    <div className="app">
      <header>
        <span className="brand">THORNHILL</span>
        <span
          className={`linkdot ${linkUp ? "up" : "down"}`}
          role="status"
          aria-live="polite"
          aria-label={linkUp ? "Backend link up" : "Backend link down"}
          title={linkUp ? "backend link up" : "backend link down"}
        />
      </header>

      <main>
      <button
        type="button"
        className={`ring ${session.toLowerCase()} ${muted ? "muted" : ""} ${connecting ? "connecting" : ""}`}
        onClick={ringTap}
        aria-label={session === "PARKED" ? "Resume voice session" : session === "PARKING" ? "Voice session is draining before park" : "Toggle mute"}
      >
        <span className="ring-label">{ringLabel}</span>
        {(session === "PARKED" || session === "PARKING") && parkReason && <span className="ring-sub">{parkReason}</span>}
      </button>

      <div className="controls">
        <button type="button" onClick={toggleMute} disabled={session === "PARKED" || session === "PARKING"}>
          {muted ? "Unmute" : "Mute"}
        </button>
        <button type="button" onClick={park} disabled={session === "PARKED" || session === "PARKING"}>
          {session === "PARKING" ? "Parking…" : "Park"}
        </button>
        <button
          type="button"
          className={pushStatus === "subscribed" ? "alerts-on" : ""}
          onClick={() => void togglePush()}
          disabled={pushBusy || pushStatus === "checking" || pushStatus === "unavailable" || pushStatus === "disabled" || pushStatus === "denied"}
          aria-pressed={pushStatus === "subscribed"}
          title={pushStatus === "denied" ? "Enable notifications in browser settings" : "Receive privacy-safe alerts while voice is parked"}
        >
          {pushBusy ? "Alerts…" : pushStatusLabel(pushStatus)}
        </button>
      </div>

      <section className="ticker" aria-label="Conversation transcript" aria-live="polite">
        {ticker.length === 0 && <div className="tick dim">transcript will appear here</div>}
        {ticker.map((line) => (
          <div key={line.id} className={`tick ${line.who}`}>
            <span className="who">{line.who === "you" ? "YOU" : "DESK"}</span> {line.text}
          </div>
        ))}
      </section>

      <section className="board" aria-labelledby="board-title">
        <h2 id="board-title">Board</h2>
        {jobList.length === 0 && <div className="dim">No jobs yet. Say what you need.</div>}
        {jobList.map((j) => (
          <article key={j.id} className={`job ${j.status}`}>
            <div className="job-head">
              <span className="job-name">{j.display_name}</span>
              <span className="job-status" role="status" aria-live="polite" aria-atomic="true">
                {j.status.replace("_", " ")}
              </span>
            </div>
            {j.status === "needs_input" && j.question && <p className="job-q">{j.question}</p>}
            {j.status === "needs_approval" && j.approvals?.[0] && (
              <div className="job-approval" role="alert">
                <strong>Approval requested</strong>
                {j.approvals[0].description && <p>{j.approvals[0].description}</p>}
                {j.approvals[0].command && <code>{j.approvals[0].command}</code>}
                {j.approvals[0].pattern_keys?.length ? <p className="dim">Scope: {j.approvals[0].pattern_keys.join(", ")}</p> : null}
                <p className="dim">Reply by voice: allow or deny once, ask for a safer alternative, or choose job/always scope. You can ask questions first.</p>
              </div>
            )}
            {j.status === "parked_approval" && j.approvals?.[0] && (
              <div className="job-approval" role="status">
                <strong>Approval parked unresolved</strong>
                {j.approvals[0].description && <p>{j.approvals[0].description}</p>}
                {j.approvals[0].command && <code>{j.approvals[0].command}</code>}
                {j.approvals[0].pattern_keys?.length ? <p className="dim">Scope: {j.approvals[0].pattern_keys.join(", ")}</p> : null}
                <p className="dim">No decision was made. The old run released its resources. Ask the desk to resume this job; any authority still needed must be requested again.</p>
              </div>
            )}
            {j.status === "failed" && j.approvals?.[0]?.state === "indeterminate" && (
              <div className="job-approval indeterminate" role="alert">
                <strong>Approval outcome indeterminate</strong>
                <p>The run was stopped. Thornhill will not retry this decision.</p>
              </div>
            )}
            {j.status === "running" && j.progress && (
              <p className="job-progress">{j.progress.state === "running" ? "Working: " : "Last: "}{j.progress.label || j.progress.tool}</p>
            )}
            {j.status === "done" && j.result_digest && <p className="job-r">{j.result_digest}</p>}
            {j.status === "failed" && j.error && <p className="job-e" role="alert">{j.error}</p>}
          </article>
        ))}
      </section>

      {notice && <div className="notice" role="alert">{notice}</div>}

      <div className="textbar">
        <input
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && sendText()}
          placeholder="Type when voice is not an option"
          aria-label="Text message to the desk"
        />
        <button type="button" onClick={sendText}>Send</button>
      </div>
      </main>

      <footer className="legal">
        <a href="https://github.com/qiviut/thornhill" target="_blank" rel="noreferrer">
          Source
        </a>
        <span>© 2026 Thornhill contributors · AGPL-3.0-only · no warranty</span>
      </footer>

      {/* biome-ignore lint/a11y/useMediaCaption: live WebRTC speech has no static caption track. */}
      <audio ref={audioRef} autoPlay />
    </div>
  );
}

function clip(arr: TranscriptLine[], line: Omit<TranscriptLine, "id">): TranscriptLine[] {
  return [...arr.slice(-7), { ...line, id: crypto.randomUUID() }];
}

function assertNever(value: never): never {
	throw new Error(`unhandled control event: ${JSON.stringify(value)}`);
}

function waitForIce(pc: RTCPeerConnection, timeoutMs: number, signal: AbortSignal): Promise<void> {
	if (signal.aborted) return Promise.resolve();
  if (pc.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    const finish = () => {
      window.clearTimeout(timer);
      pc.removeEventListener("icegatheringstatechange", onChange);
      signal.removeEventListener("abort", finish);
      resolve();
    };
    const onChange = () => {
      if (pc.iceGatheringState === "complete") {
        finish();
      }
    };
    const timer = window.setTimeout(finish, timeoutMs);
    pc.addEventListener("icegatheringstatechange", onChange);
    signal.addEventListener("abort", finish, { once: true });
  });
}

async function waitForRemoteAudioDrain(
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

function abortableDelay(delayMs: number, signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.resolve();
  return new Promise((resolve) => {
    const finish = () => {
      window.clearTimeout(timer);
      signal.removeEventListener("abort", finish);
      resolve();
    };
    const timer = window.setTimeout(finish, delayMs);
    signal.addEventListener("abort", finish, { once: true });
  });
}
