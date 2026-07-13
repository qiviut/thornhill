import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ControlLink, type BusEvent } from "./ws";
import { earcons, playPrebaked, prefetchPrebaked, unlockAudio } from "./earcons";

type SessionState = "PARKED" | "PARKING" | "QUIET" | "LIVE";

interface Approval {
  id: string;
  state: "pending" | "sending" | "indeterminate";
  description?: string;
  command?: string;
  pattern_keys?: string[];
  allow_permanent: boolean;
}

interface Progress {
  tool?: string;
  label?: string;
  state: string;
  updated_at: string;
}

interface Job {
  id: string;
  display_name: string;
  status: string;
  question?: string;
  approvals?: Approval[];
  progress?: Progress;
  result_digest?: string;
  error?: string;
}

interface TranscriptLine {
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

  const pcRef = useRef<RTCPeerConnection | null>(null);
  const micRef = useRef<MediaStream | null>(null);
  const audioRef = useRef<HTMLAudioElement | null>(null);
  const linkRef = useRef<ControlLink | null>(null);
  const mutedRef = useRef(false);
  const sessionRef = useRef<SessionState>("PARKED");
  sessionRef.current = session;
  mutedRef.current = muted;

  const pushState = useCallback(() => {
    linkRef.current?.sendState(mutedRef.current, document.hidden);
  }, []);

  const teardownCall = useCallback(() => {
    pcRef.current?.close();
    pcRef.current = null;
    micRef.current?.getTracks().forEach((t) => t.stop());
    micRef.current = null;
    if (audioRef.current) audioRef.current.srcObject = null;
  }, []);

  const connect = useCallback(async () => {
    if (pcRef.current || connectingRef.current) return;
    connectingRef.current = true;
    setConnecting(true);
    setNotice("");
    unlockAudio();
    prefetchPrebaked(PREBAKE_KEYS);
    try {
      const mic = await navigator.mediaDevices.getUserMedia({ audio: true });
      micRef.current = mic;
      const pc = new RTCPeerConnection();
      pcRef.current = pc;
      for (const track of mic.getTracks()) {
        track.enabled = !mutedRef.current;
        pc.addTrack(track, mic);
      }
      pc.ontrack = (ev) => {
        if (audioRef.current) audioRef.current.srcObject = ev.streams[0];
      };
      pc.onconnectionstatechange = () => {
        console.debug("rtc state", pc.connectionState);
        if (pc.connectionState === "failed") {
          earcons.linkLost();
          teardownCall();
        }
      };

      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      await waitForIce(pc, 1500);

      const resp = await fetch("/offer", {
        method: "POST",
        headers: { "Content-Type": "application/sdp" },
        body: pc.localDescription?.sdp ?? offer.sdp ?? "",
        signal: AbortSignal.timeout(15_000),
      });
      if (!resp.ok) {
        throw new Error(`offer relay: http ${resp.status}`);
      }
      const answer = await resp.text();
      await pc.setRemoteDescription({ type: "answer", sdp: answer });
      pushState();
      console.debug("call up");
    } catch (err) {
      console.warn("connect failed", err);
      setNotice(String(err));
      teardownCall();
      void playPrebaked("voice_down");
    } finally {
      connectingRef.current = false;
      setConnecting(false);
    }
  }, [pushState, teardownCall]);

  const finishPark = useCallback(async (reason: string) => {
    const parkedPC = pcRef.current;
    if (parkedPC) {
      await waitForRemoteAudioDrain(parkedPC, 2500, 300);
      // A replacement call or newer server state must never be torn down by a
      // stale park completion.
      if (pcRef.current !== parkedPC || sessionRef.current !== "PARKING") return;
    }
    if (sessionRef.current !== "PARKING") return;
    sessionRef.current = "PARKED";
    setSession("PARKED");
    setParkReason(reason);
    teardownCall();
    if (reason === "rollover") {
      // Invisible re-materialization: same browser permission, new call.
      window.setTimeout(() => void connect(), 250);
    }
  }, [connect, teardownCall]);

  const handleEvent = useCallback(
    (e: BusEvent) => {
      const p = (e.payload ?? {}) as Record<string, unknown>;
      switch (e.kind) {
        case "session.state": {
          const st = (p.state as SessionState) ?? "PARKED";
          const reason = (p.reason as string) ?? "";
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
          sessionRef.current = st;
          setSession(st);
          setParkReason(reason);
          if (st === "PARKING") {
            // Server-originated park requests (including the voice tool) must
            // stop new input just like the Park button while remote audio drains.
            micRef.current?.getAudioTracks().forEach((track) => (track.enabled = false));
          }
          break;
        }
        case "transcript.user":
          setTicker((t) => clip(t, { who: "you", text: String(p.text ?? "") }));
          break;
        case "transcript.assistant":
          setTicker((t) => clip(t, { who: "desk", text: String(p.text ?? "") }));
          break;
        case "job.queued":
        case "job.running":
        case "job.needs_input":
        case "job.needs_approval":
        case "job.approval_resolved":
        case "job.progress":
        case "job.done":
        case "job.failed":
        case "job.cancelled": {
          const job = p as unknown as Job;
          if (!e.job_id) break;
          setJobs((m) => {
            const next = new Map(m);
            // Job lifecycle events carry complete snapshots. Replace rather
            // than merge so omitted cleared fields (approvals/progress/error)
            // cannot survive from an earlier state.
            next.set(e.job_id!, { ...job, id: e.job_id! });
            return next;
          });
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
          if (!e.job_id) break;
          const to = String(p.to ?? "");
          setJobs((m) => {
            const next = new Map(m);
            const prev = next.get(e.job_id!);
            if (prev) next.set(e.job_id!, { ...prev, display_name: to });
            return next;
          });
          break;
        }
        case "error.voice": {
          const msg = String(p.message ?? "voice error");
          setNotice(msg);
          const key = p.play as string | undefined;
          if (key) void playPrebaked(key);
          break;
        }
        default:
          break;
      }
    },
    [finishPark],
  );

  useEffect(() => {
    const link = new ControlLink({
      onEvent: handleEvent,
      onLink: (up) => {
        setLinkUp(up);
        if (up) earcons.linkOk();
        else earcons.linkLost();
      },
    });
    linkRef.current = link;
    link.start();
    const vis = () => pushState();
    document.addEventListener("visibilitychange", vis);
    return () => {
      document.removeEventListener("visibilitychange", vis);
      link.stop();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const toggleMute = useCallback(() => {
    setMuted((m) => {
      const next = !m;
      micRef.current?.getAudioTracks().forEach((t) => (t.enabled = !next));
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
    micRef.current?.getAudioTracks().forEach((track) => (track.enabled = false));
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
    const rank: Record<string, number> = { needs_approval: 0, needs_input: 1, running: 2, queued: 3, failed: 4, done: 5, cancelled: 6 };
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
        <span className={`linkdot ${linkUp ? "up" : "down"}`} title={linkUp ? "backend link up" : "backend link down"} />
      </header>

      <button
        className={`ring ${session.toLowerCase()} ${muted ? "muted" : ""} ${connecting ? "connecting" : ""}`}
        onClick={ringTap}
        aria-label={session === "PARKED" ? "Resume voice session" : session === "PARKING" ? "Voice session is draining before park" : "Toggle mute"}
      >
        <span className="ring-label">{ringLabel}</span>
        {(session === "PARKED" || session === "PARKING") && parkReason && <span className="ring-sub">{parkReason}</span>}
      </button>

      <div className="controls">
        <button onClick={toggleMute} disabled={session === "PARKED" || session === "PARKING"}>
          {muted ? "Unmute" : "Mute"}
        </button>
        <button onClick={park} disabled={session === "PARKED" || session === "PARKING"}>
          {session === "PARKING" ? "Parking…" : "Park"}
        </button>
      </div>

      <section className="ticker" aria-live="polite">
        {ticker.length === 0 && <div className="tick dim">transcript will appear here</div>}
        {ticker.map((l, i) => (
          <div key={i} className={`tick ${l.who}`}>
            <span className="who">{l.who === "you" ? "YOU" : "DESK"}</span> {l.text}
          </div>
        ))}
      </section>

      <section className="board">
        <h2>Board</h2>
        {jobList.length === 0 && <div className="dim">No jobs yet. Say what you need.</div>}
        {jobList.map((j) => (
          <article key={j.id} className={`job ${j.status}`}>
            <div className="job-head">
              <span className="job-name">{j.display_name}</span>
              <span className="job-status">{j.status.replace("_", " ")}</span>
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
            {j.status === "failed" && j.error && <p className="job-e">{j.error}</p>}
          </article>
        ))}
      </section>

      {notice && <div className="notice">{notice}</div>}

      <div className="textbar">
        <input
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && sendText()}
          placeholder="Type when voice is not an option"
          aria-label="Text message to the desk"
        />
        <button onClick={sendText}>Send</button>
      </div>

      <audio ref={audioRef} autoPlay />
    </div>
  );
}

function clip(arr: TranscriptLine[], line: TranscriptLine): TranscriptLine[] {
  return [...arr.slice(-7), line];
}

function waitForIce(pc: RTCPeerConnection, timeoutMs: number): Promise<void> {
  if (pc.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    const timer = window.setTimeout(resolve, timeoutMs);
    pc.addEventListener("icegatheringstatechange", () => {
      if (pc.iceGatheringState === "complete") {
        window.clearTimeout(timer);
        resolve();
      }
    });
  });
}

async function waitForRemoteAudioDrain(
  pc: RTCPeerConnection,
  maxWaitMs: number,
  quietWindowMs: number,
): Promise<void> {
  const started = performance.now();
  let quietSince = started;
  while (performance.now() - started < maxWaitMs) {
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
    await new Promise((resolve) => window.setTimeout(resolve, 50));
  }
}
