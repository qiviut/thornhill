// Session lifecycle FSM. Pure and clock-injected so the timer ladder is
// unit-testable without a WebSocket in sight.
//
//	LIVE ──quiet_after──▶ QUIET ──park request / idle──▶ PARKING ──drained──▶ PARKED
//	  ▲                     │speech                                                     │
//	  └─────────────────────┘                                  resume (new call) ──────┘
//
// PARKED is the canonical persistent state; a live call is a disposable
// materialization of it. Guards: never park mid-response, while output audio
// is draining, during operator speech, or mid-tool round-trip; honor a grace
// window after a needs_input question was voiced.
package desk

import (
	"sync"
	"time"
)

type State string

const (
	StateLive    State = "LIVE"
	StateQuiet   State = "QUIET"
	StateParking State = "PARKING"
	StateParked  State = "PARKED"
)

type ParkReason string

const (
	ParkIdle         ParkReason = "idle"
	ParkMuteHidden   ParkReason = "mute_hidden"
	ParkExplicit     ParkReason = "explicit"
	ParkDisconnect   ParkReason = "disconnect"
	ParkRollover     ParkReason = "rollover"
	ParkDrainTimeout ParkReason = "drain_timeout"
)

type FSMConfig struct {
	QuietAfter time.Duration
	ParkAfter  time.Duration
	RolloverAt time.Duration
	// MuteParkAfter: mute alone is announce-only mode; mute this long
	// (or mute while the tab is hidden) parks.
	MuteParkAfter time.Duration
	// Grace after voicing a needs_input question before idle-parking.
	QuestionGrace time.Duration
	// Maximum time to wait for disposable Realtime generation/audio/tool work
	// to drain. Durable jobs are not cancelled when this fallback fires.
	ParkDrainAfter time.Duration
}

func DefaultFSMConfig(quiet, park, rollover time.Duration) FSMConfig {
	return FSMConfig{
		QuietAfter:     quiet,
		ParkAfter:      park,
		RolloverAt:     rollover,
		MuteParkAfter:  10 * time.Minute,
		QuestionGrace:  2 * time.Minute,
		ParkDrainAfter: 30 * time.Second,
	}
}

type FSM struct {
	mu  sync.Mutex
	cfg FSMConfig

	state           State
	callStart       time.Time
	lastSpeech      time.Time // either party
	inFlight        bool      // a response has been requested or is generating
	audioPlaying    bool      // WebRTC output has not drained yet
	userSpeaking    bool      // VAD has an open operator turn
	toolPending     bool      // a function call awaits its output
	muted           bool
	hidden          bool
	mutedSince      time.Time
	graceUntil      time.Time
	parkRequested   bool
	parkRequestedAt time.Time
	parkReason      ParkReason
}

func NewFSM(cfg FSMConfig) *FSM {
	return &FSM{cfg: cfg, state: StateParked}
}

func (f *FSM) State() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

// CallStarted: a WebRTC call materialized; we are LIVE.
func (f *FSM) CallStarted(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = StateLive
	f.callStart = now
	f.lastSpeech = now
	f.inFlight = false
	f.audioPlaying = false
	f.userSpeaking = false
	f.toolPending = false
	f.graceUntil = time.Time{}
	f.parkRequested = false
	f.parkRequestedAt = time.Time{}
	f.parkReason = ""
}

func (f *FSM) speechActivityLocked(now time.Time) {
	f.lastSpeech = now
	if f.state == StateQuiet {
		f.state = StateLive
	}
}

func (f *FSM) SpeechActivity(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.speechActivityLocked(now)
}

func (f *FSM) SpeechStarted(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userSpeaking = true
	f.speechActivityLocked(now)
}

func (f *FSM) SpeechStopped(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userSpeaking = false
	f.speechActivityLocked(now)
}

func (f *FSM) ResponseStarted(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight = true
	f.lastSpeech = now
}

// TryStartResponse atomically admits a Desk-owned response and marks it
// in-flight. RequestPark uses the same mutex, so PARKING cannot be inserted
// between response admission and its busy-state transition.
func (f *FSM) TryStartResponse(now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != StateLive && f.state != StateQuiet {
		return false
	}
	if f.inFlight || f.audioPlaying || f.userSpeaking || f.toolPending {
		return false
	}
	f.inFlight = true
	f.lastSpeech = now
	return true
}

func (f *FSM) ResponseDone(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight = false
	f.lastSpeech = now
}

func (f *FSM) AudioStarted(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audioPlaying = true
	f.lastSpeech = now
}

func (f *FSM) AudioStopped(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audioPlaying = false
	f.lastSpeech = now
}

func (f *FSM) ToolPending(p bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolPending = p
}

func (f *FSM) Busy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight || f.audioPlaying || f.userSpeaking || f.toolPending
}

// CanStartResponse is a read-only readiness probe. The actual transition uses
// TryStartResponse; callers must not treat this probe as response admission.
func (f *FSM) CanStartResponse() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != StateLive && f.state != StateQuiet {
		return false
	}
	return !f.inFlight && !f.audioPlaying && !f.userSpeaking && !f.toolPending
}

// QuestionVoiced arms the thinking-time grace window.
func (f *FSM) QuestionVoiced(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.graceUntil = now.Add(f.cfg.QuestionGrace)
}

func (f *FSM) SetClientState(now time.Time, muted, hidden bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if muted && !f.muted {
		f.mutedSince = now
	}
	f.muted, f.hidden = muted, hidden
}

// Disconnected: the browser or call dropped. Crash-only: same landing as a
// deliberate park.
func (f *FSM) Disconnected() ParkReason {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = StateParked
	return ParkDisconnect
}

// RequestPark enters a visible draining state. Tick completes the park only
// after generation, playback, operator speech, and tool output have all ended.
func (f *FSM) RequestPark(now time.Time, reason ParkReason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == StateParked {
		return
	}
	f.parkRequested = true
	f.parkRequestedAt = now
	f.parkReason = reason
	f.state = StateParking
}

// ExplicitPark is the forced shutdown path for context cancellation. Operator
// park requests use RequestPark so audible output can drain first.
func (f *FSM) ExplicitPark() ParkReason {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = StateParked
	return ParkExplicit
}

// Tick evaluates the timer ladder. Returns (park, reason, becameQuiet).
func (f *FSM) Tick(now time.Time) (bool, ParkReason, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == StateParked {
		return false, "", false
	}
	busy := f.inFlight || f.audioPlaying || f.userSpeaking || f.toolPending
	inGrace := now.Before(f.graceUntil)

	if f.parkRequested {
		if busy {
			if f.cfg.ParkDrainAfter > 0 && now.Sub(f.parkRequestedAt) >= f.cfg.ParkDrainAfter {
				f.state = StateParked
				f.parkRequested = false
				f.parkRequestedAt = time.Time{}
				f.parkReason = ""
				return true, ParkDrainTimeout, false
			}
			return false, "", false
		}
		reason := f.parkReason
		if reason == "" {
			reason = ParkExplicit
		}
		f.state = StateParked
		f.parkRequested = false
		f.parkRequestedAt = time.Time{}
		f.parkReason = ""
		return true, reason, false
	}

	// Rollover beats everything except an in-flight utterance; the desk
	// re-materializes immediately, so this is invisible.
	if now.Sub(f.callStart) >= f.cfg.RolloverAt && !busy {
		f.state = StateParked
		return true, ParkRollover, false
	}

	if !busy && !inGrace {
		if f.muted && (f.hidden || now.Sub(f.mutedSince) >= f.cfg.MuteParkAfter) {
			f.state = StateParked
			return true, ParkMuteHidden, false
		}
		if now.Sub(f.lastSpeech) >= f.cfg.ParkAfter {
			f.state = StateParked
			return true, ParkIdle, false
		}
	}

	if f.state == StateLive && !busy && now.Sub(f.lastSpeech) >= f.cfg.QuietAfter {
		f.state = StateQuiet
		return false, "", true
	}
	return false, "", false
}
