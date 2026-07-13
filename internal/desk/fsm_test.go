package desk

import (
	"testing"
	"time"
)

func cfg() FSMConfig {
	return DefaultFSMConfig(30*time.Second, 10*time.Minute, 57*time.Minute)
}

func TestQuietAfter30sThenSpeechRevives(t *testing.T) {
	f := NewFSM(cfg())
	t0 := time.Unix(1000, 0)
	f.CallStarted(t0)

	if park, _, quiet := f.Tick(t0.Add(29 * time.Second)); park || quiet {
		t.Fatalf("premature transition at 29s: park=%v quiet=%v", park, quiet)
	}
	park, _, quiet := f.Tick(t0.Add(31 * time.Second))
	if park || !quiet || f.State() != StateQuiet {
		t.Fatalf("expected QUIET at 31s, got state=%s park=%v quiet=%v", f.State(), park, quiet)
	}
	f.SpeechActivity(t0.Add(40 * time.Second))
	if f.State() != StateLive {
		t.Fatalf("speech should revive to LIVE, got %s", f.State())
	}
}

func TestParkAfterIdle(t *testing.T) {
	f := NewFSM(cfg())
	t0 := time.Unix(1000, 0)
	f.CallStarted(t0)
	f.Tick(t0.Add(31 * time.Second)) // -> QUIET
	park, why, _ := f.Tick(t0.Add(10*time.Minute + time.Second))
	if !park || why != ParkIdle || f.State() != StateParked {
		t.Fatalf("expected idle park, got park=%v why=%s state=%s", park, why, f.State())
	}
}

func TestNoParkWhileResponseInFlight(t *testing.T) {
	f := NewFSM(cfg())
	t0 := time.Unix(1000, 0)
	f.CallStarted(t0)
	f.ResponseStarted(t0.Add(time.Second))
	// Way past every timer, but a response is in flight.
	park, _, _ := f.Tick(t0.Add(20 * time.Minute))
	if park {
		t.Fatal("must not park mid-response")
	}
	f.ResponseDone(t0.Add(20 * time.Minute))
	park, why, _ := f.Tick(t0.Add(31 * time.Minute))
	if !park || why != ParkIdle {
		t.Fatalf("expected idle park after response done, got park=%v why=%s", park, why)
	}
}

func TestNoParkUntilOutputAudioDrains(t *testing.T) {
	f := NewFSM(cfg())
	t0 := time.Unix(1000, 0)
	f.CallStarted(t0)
	f.ResponseStarted(t0.Add(time.Second))
	f.AudioStarted(t0.Add(2 * time.Second))
	f.ResponseDone(t0.Add(3 * time.Second))
	if park, _, _ := f.Tick(t0.Add(20 * time.Minute)); park {
		t.Fatal("response.done must not park while WebRTC output is still playing")
	}
	f.AudioStopped(t0.Add(20 * time.Minute))
	if park, why, _ := f.Tick(t0.Add(31 * time.Minute)); !park || why != ParkIdle {
		t.Fatalf("expected idle park after audio drained, got park=%v why=%s", park, why)
	}
}

func TestRequestedParkPublishesParkingStateUntilOutputDrains(t *testing.T) {
	f := NewFSM(cfg())
	t0 := time.Unix(1000, 0)
	f.CallStarted(t0)
	f.ResponseStarted(t0.Add(time.Second))
	f.AudioStarted(t0.Add(2 * time.Second))
	f.RequestPark(t0.Add(2*time.Second), ParkExplicit)
	if f.State() != StateParking {
		t.Fatalf("requested park state = %s, want PARKING", f.State())
	}
	f.ResponseDone(t0.Add(3 * time.Second))
	if park, _, _ := f.Tick(t0.Add(4 * time.Second)); park || f.State() != StateParking {
		t.Fatalf("park completed before audio drain: park=%v state=%s", park, f.State())
	}
	f.AudioStopped(t0.Add(5 * time.Second))
	if park, why, _ := f.Tick(t0.Add(6 * time.Second)); !park || why != ParkExplicit || f.State() != StateParked {
		t.Fatalf("drained park = %v/%s state=%s", park, why, f.State())
	}
}

func TestRequestedParkWaitsForToolOutput(t *testing.T) {
	f := NewFSM(cfg())
	t0 := time.Unix(1000, 0)
	f.CallStarted(t0)
	f.ToolPending(true)
	f.RequestPark(t0, ParkExplicit)
	if park, _, _ := f.Tick(t0.Add(time.Second)); park {
		t.Fatal("park completed while function output was pending")
	}
	f.ToolPending(false)
	if park, why, _ := f.Tick(t0.Add(2 * time.Second)); !park || why != ParkExplicit {
		t.Fatalf("park after function output = %v/%s", park, why)
	}
}

func TestRequestedParkHasBoundedDisposableDrain(t *testing.T) {
	c := cfg()
	c.ParkDrainAfter = 5 * time.Second
	f := NewFSM(c)
	t0 := time.Unix(1000, 0)
	f.CallStarted(t0)
	f.ToolPending(true)
	f.RequestPark(t0, ParkExplicit)
	if park, _, _ := f.Tick(t0.Add(4 * time.Second)); park {
		t.Fatal("Park timed out before its drain bound")
	}
	if park, why, _ := f.Tick(t0.Add(5 * time.Second)); !park || why != ParkDrainTimeout {
		t.Fatalf("bounded drain = %v/%s, want true/%s", park, why, ParkDrainTimeout)
	}
}

func TestNoParkWhileOperatorIsSpeaking(t *testing.T) {
	f := NewFSM(cfg())
	t0 := time.Unix(1000, 0)
	f.CallStarted(t0)
	f.SpeechStarted(t0.Add(time.Second))
	if park, _, _ := f.Tick(t0.Add(20 * time.Minute)); park {
		t.Fatal("must not park during an open VAD speech turn")
	}
	f.SpeechStopped(t0.Add(20 * time.Minute))
	if park, why, _ := f.Tick(t0.Add(31 * time.Minute)); !park || why != ParkIdle {
		t.Fatalf("expected idle park after speech stopped, got park=%v why=%s", park, why)
	}
}

func TestNoParkDuringToolRoundTrip(t *testing.T) {
	f := NewFSM(cfg())
	t0 := time.Unix(1000, 0)
	f.CallStarted(t0)
	f.ToolPending(true)
	if park, _, _ := f.Tick(t0.Add(15 * time.Minute)); park {
		t.Fatal("must not park mid-tool")
	}
}

func TestQuestionGraceHoldsIdlePark(t *testing.T) {
	f := NewFSM(cfg())
	t0 := time.Unix(1000, 0)
	f.CallStarted(t0)
	// Idle threshold reached, but a question was just voiced.
	f.QuestionVoiced(t0.Add(10 * time.Minute))
	if park, _, _ := f.Tick(t0.Add(10*time.Minute + 30*time.Second)); park {
		t.Fatal("grace window must hold the park")
	}
	if park, why, _ := f.Tick(t0.Add(13 * time.Minute)); !park || why != ParkIdle {
		t.Fatalf("expected park after grace, got park=%v why=%s", park, why)
	}
}

func TestMuteAloneIsAnnounceOnlyMuteHiddenParks(t *testing.T) {
	f := NewFSM(cfg())
	t0 := time.Unix(1000, 0)
	f.CallStarted(t0)
	f.SetClientState(t0.Add(time.Minute), true, false) // muted, visible
	// Mute alone before MuteParkAfter and before idle threshold: no park.
	if park, _, _ := f.Tick(t0.Add(3 * time.Minute)); park {
		t.Fatal("mute alone must not park early")
	}
	// Hide the tab: parks immediately at next tick.
	f.SetClientState(t0.Add(4*time.Minute), true, true)
	park, why, _ := f.Tick(t0.Add(4*time.Minute + time.Second))
	if !park || why != ParkMuteHidden {
		t.Fatalf("expected mute+hidden park, got park=%v why=%s", park, why)
	}
}

func TestMuteLongParksEvenIfVisible(t *testing.T) {
	f := NewFSM(cfg())
	t0 := time.Unix(1000, 0)
	f.CallStarted(t0)
	f.SetClientState(t0, true, false)
	// Keep "speech" fresh so idle park does not fire first (announce-only
	// mode means the assistant may keep talking).
	f.SpeechActivity(t0.Add(9 * time.Minute))
	park, why, _ := f.Tick(t0.Add(10*time.Minute + time.Second))
	if !park || why != ParkMuteHidden {
		t.Fatalf("expected long-mute park, got park=%v why=%s", park, why)
	}
}

func TestRolloverBeatsIdleAndFiresNearWallClock(t *testing.T) {
	c := cfg()
	f := NewFSM(c)
	t0 := time.Unix(1000, 0)
	f.CallStarted(t0)
	// Keep the session chatty so idle never fires.
	for m := 1; m < 57; m++ {
		f.SpeechActivity(t0.Add(time.Duration(m) * time.Minute))
		if park, why, _ := f.Tick(t0.Add(time.Duration(m)*time.Minute + time.Second)); park {
			t.Fatalf("unexpected park at minute %d: %s", m, why)
		}
	}
	park, why, _ := f.Tick(t0.Add(57*time.Minute + time.Second))
	if !park || why != ParkRollover {
		t.Fatalf("expected rollover park, got park=%v why=%s", park, why)
	}
}

func TestExplicitParkAndDisconnect(t *testing.T) {
	f := NewFSM(cfg())
	f.CallStarted(time.Unix(1000, 0))
	if r := f.ExplicitPark(); r != ParkExplicit || f.State() != StateParked {
		t.Fatalf("explicit park: %s / %s", r, f.State())
	}
	f2 := NewFSM(cfg())
	f2.CallStarted(time.Unix(1000, 0))
	if r := f2.Disconnected(); r != ParkDisconnect || f2.State() != StateParked {
		t.Fatalf("disconnect park: %s / %s", r, f2.State())
	}
}

func TestParkedTickIsInert(t *testing.T) {
	f := NewFSM(cfg())
	if park, _, quiet := f.Tick(time.Unix(2000, 0)); park || quiet {
		t.Fatal("parked FSM must be inert")
	}
}
