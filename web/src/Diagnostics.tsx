import { useEffect, useRef, useState } from "react";
import { testSound } from "./earcons";

interface DiagnosticsProps {
  linkUp: boolean;
}

export function Diagnostics({ linkUp }: DiagnosticsProps) {
  const [online, setOnline] = useState(navigator.onLine);
  const [toneResult, setToneResult] = useState("");
  const [speechResult, setSpeechResult] = useState("");
  const audioRef = useRef<HTMLAudioElement | null>(null);
  const generation = useRef(0);
  const toneGeneration = useRef(0);

  useEffect(() => {
    const update = () => setOnline(navigator.onLine);
    window.addEventListener("online", update);
    window.addEventListener("offline", update);
    return () => {
      window.removeEventListener("online", update);
      window.removeEventListener("offline", update);
      generation.current += 1;
      toneGeneration.current += 1;
      audioRef.current?.pause();
      audioRef.current = null;
    };
  }, []);

  const playTone = async () => {
    const current = ++toneGeneration.current;
    setToneResult("Starting test tone…");
    try {
      await testSound();
      if (current === toneGeneration.current) setToneResult("Tone started. Did you hear two notes?");
    } catch {
      if (current === toneGeneration.current) setToneResult("Could not start the tone. Check this browser's sound permissions and output device.");
    }
  };

  const playSpeech = async () => {
    const current = ++generation.current;
    audioRef.current?.pause();
    setSpeechResult("Starting test speech…");
    try {
      const audio = new Audio("/audio/prebaked/resume_ok.wav");
      audioRef.current = audio;
      // Invoke play directly in the user gesture, before awaiting anything.
      await audio.play();
      if (current === generation.current) setSpeechResult("Speech playback started. Did you hear a voice?");
    } catch {
      if (current === generation.current) setSpeechResult("Could not play test speech. Check browser sound permissions and the backend connection.");
    }
  };

  return (
    <details className="diagnostics">
      <summary>Diagnostics</summary>
      <p>Test your browser's sound output without starting a voice session.</p>
      <div className="diagnostic-buttons">
        <button type="button" onClick={() => void playTone()}>Test sound</button>
        <button type="button" onClick={() => void playSpeech()}>Test speech</button>
      </div>
      <p role="status" aria-live="polite">{toneResult}</p>
      <p role="status" aria-live="polite">{speechResult}</p>
      <p>Browser network: {online ? "online" : "offline"} · Control link: {linkUp ? "connected" : "disconnected"}</p>
      <p className="dim">These indicators do not test the live voice connection or prove sound was audible. If playback starts but you hear nothing, check the browser tab's mute setting and output device.</p>
    </details>
  );
}
