# Thornhill design decisions (v0.1 → v0.4)

Log of the design iteration that produced this repo, 2026-07-08/09.
Kept terse; the reasoning that matters for maintenance.

## Topology

- **Sideband pattern** (v0.2): browser WebRTC directly to OpenAI for
  audio; server relays only SDP signaling and attaches a WebSocket via
  `call_id` for control. Rejected: full server audio relay (latency,
  work), MCP `server_url` tools (would require OpenAI reaching into the
  tailnet).
- **One API key, no minting** (v0.4): the unified `/v1/realtime/calls`
  interface lets the server create calls with the org key, so ephemeral
  client secrets were dropped entirely. Key never reaches the browser.
- **No LiteLLM**: client ↔ this app ↔ Hermes ↔ OpenAI, nothing between.
  Per-leg usage is recorded from `response.done` and completion usage
  fields into our own ledger.

## Hermes as agent, not model

- Workers are Hermes Agent instances (Nous Research harness) configured
  with OpenAI models — the GPT-Live delegation architecture with an open
  back office.
- **A job is a conversation with a Hermes session** over the instance's
  OpenAI-compatible API server. `needs_input` is just an agent turn
  ending in a question; the operator's spoken answer is the next user
  message. Session key = job ULID.
- Knowledge boundary: the board is ephemeral orchestration state only.
  Durable knowledge lives Hermes-side (Obsidian, beads/kanban, Curator,
  filesystem). Jobs carry references out, never content in.
- Swarm job kind (flywheel runs: plan → beads → bd burn-down as
  progress) is a v1.1 seam, deliberately cheap now.
- Standing patrols run in Hermes cron; Thornhill keeps only the voice
  etiquette (whether/when results become speech vs board).

## Session lifecycle

- **Crash-only**: PARKED is canonical; the live call is a cache of it.
  Network drop, iOS tab kill, 60-min cap, deliberate pause — one code
  path. The rolling debrief is maintained continuously (not compiled at
  park time) so ungraceful exits lose nothing.
- Timer ladder: QUIET at 30s (conversational posture, model told to
  stay silent — per operator, matches real speech), PARK at 10m,
  rollover at 57m with auto-redial. GATE (~3min, client-side mic
  disable + local VAD wake) deferred as experiment E1.
- Mute alone = announce-only mode. Mute+hidden or mute>10m parks.
- Guards: never park mid-response/mid-tool; 2-min grace after a voiced
  job question.

## Voice desk behavior

- Desk toolset is minimal-privilege by design (dispatch, status,
  answer, cancel, rename, park, wait_for_user): agent outputs flowing
  into the realtime context can at worst spawn or cancel a job. The
  dangerous capabilities live in Hermes on isolated fleet VMs.
- Proactivity v1: lifecycle announcements + needs_input relay only,
  injected as system messages at lulls, ≥10s apart, single-writer turn
  loop. Interjection *feel* is experiment E5.
- Naming: `job_id` ULID immutable; `display_name` mutable, christened
  by the desk model at dispatch, renamed on drift. Names are the voice
  handles; ambiguity is surfaced (`ErrAmbiguous`), never guessed.
- Debrief greeting is scale-calibrated in the prompt: nothing → five
  words; one thing → one sentence; pile → headline count, offer rundown.

## Error ladder

- L0 model voices errors (injected system items), L1 prebaked/dynamic
  OpenAI TTS over the control WS, L2 browser-local earcons on heartbeat
  loss. The initial mic tap unlocks WebAudio for all rungs.

## Stack

- Go monolith: net/http (1.22+ routing), coder/websocket, pgx/v5,
  River (durable queue, MaxAttempts=2 — no retry storms against
  agents), oklog/ulid. Plain listener; the host is already on the
  tailnet (tsnet seam noted, dep weight not earned).
- Frontend: Vite + React + TS, raw WebRTC (~50 lines; no SDK for this
  odd sideband topology), plain CSS, WebAudio earcons.
- Postgres 17; schema is four tables (jobs, event_log, summaries,
  usage_ledger).

## Deliberately deferred (experiments)

- E1 GATE wake-on-voice: mic-track disable + local VAD; measure onset
  clipping; can `input_audio_buffer.append` splice a ring buffer into a
  WebRTC-fed session?
- E2 preamble/narration quality at `reasoning.effort=low`; tune the
  prompt after hearing it.
- E3 Hermes API-server long-run semantics: stream lifetime vs hooks,
  session resume mapping, hook payload shapes.
- E4 iOS Safari: control-WS survival in background, park/resume
  timings, auto-redial after rollover without a fresh gesture.
- E5 interjection feel and budget tuning.
