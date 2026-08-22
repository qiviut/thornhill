package dispatch

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"thornhill/internal/events"
	"thornhill/internal/store"
)

func TestCancelledJobEventRedactsApprovalNonce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus := events.NewBus(nil, slog.Default())
	eventCh := bus.Subscribe(ctx, false)
	const nonce = "cancelled-approval-nonce"
	j := store.Job{
		ID: "cancelled-job", Status: store.StatusCancelled,
		Approvals: []store.Approval{{ID: "approval", DecisionNonce: nonce, State: store.ApprovalStatePending}},
	}

	publishCancelledJob(bus, j)
	e := <-eventCh
	if e.Kind != events.KindJobCancelled || e.JobID != j.ID {
		t.Fatalf("event = %+v", e)
	}
	var published store.Job
	if err := json.Unmarshal(e.Payload, &published); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	if len(published.Approvals) != 1 || published.Approvals[0].DecisionNonce != "" {
		t.Fatalf("cancelled event retained approval nonce: %+v", published.Approvals)
	}
	if j.Approvals[0].DecisionNonce != nonce {
		t.Fatalf("redaction mutated durable snapshot: %+v", j.Approvals)
	}
}

func TestQueuedJobEventRedactsApprovalNonce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus := events.NewBus(nil, slog.Default())
	eventCh := bus.Subscribe(ctx, false)
	const nonce = "queued-approval-nonce"
	j := store.Job{
		ID: "queued-job", Status: store.StatusQueued,
		Approvals: []store.Approval{{ID: "approval", DecisionNonce: nonce, State: store.ApprovalStatePending}},
	}

	publishQueuedJob(bus, j)
	e := <-eventCh
	if e.Kind != events.KindJobQueued || e.JobID != j.ID {
		t.Fatalf("event = %+v", e)
	}
	var published store.Job
	if err := json.Unmarshal(e.Payload, &published); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	if len(published.Approvals) != 1 || published.Approvals[0].DecisionNonce != "" {
		t.Fatalf("queued event retained approval nonce: %+v", published.Approvals)
	}
	if j.Approvals[0].DecisionNonce != nonce {
		t.Fatalf("redaction mutated durable snapshot: %+v", j.Approvals)
	}
}

func TestPrepareForResumePreservesFailureEvidence(t *testing.T) {
	finished := time.Unix(1_700_000_000, 0)
	progress := &store.Progress{Tool: "terminal", State: "running", Label: "editing config"}
	j := store.Job{
		Status:          store.StatusFailed,
		Question:        "old question",
		ResultDigest:    "partial result",
		Error:           "worker interrupted",
		HermesSessionID: "durable-session",
		HermesRunID:     "stale-run",
		Approvals:       []store.Approval{{ID: "stale-approval", State: "pending"}},
		Progress:        progress,
		FinishedAt:      &finished,
	}

	if !prepareForResume(&j) {
		t.Fatal("failed job was not claimed for resume")
	}

	if j.Status != store.StatusQueued || j.Question != "" || j.ResultDigest != "" ||
		j.HermesRunID != "" || len(j.Approvals) != 0 || j.FinishedAt != nil {
		t.Fatalf("resume reset left stale execution state: %+v", j)
	}
	if j.HermesSessionID != "durable-session" || j.Error != "worker interrupted" || j.Progress != progress {
		t.Fatalf("resume reset discarded durable evidence: %+v", j)
	}
}

func TestPrepareForResumePreservesParkedApprovalUntilRunSnapshotsIt(t *testing.T) {
	parkedAt := time.Unix(1_700_000_001, 0).UTC()
	approval := store.Approval{
		ID: "parked-approval", DecisionNonce: "stale-nonce", State: store.ApprovalStateParked,
		Description: "restart service", PatternKeys: []string{"service restart"}, ParkedAt: &parkedAt,
	}
	j := store.Job{
		Status: store.StatusParkedApproval, HermesSessionID: "durable-session", HermesRunID: "stopped-run",
		Approvals: []store.Approval{approval}, Progress: &store.Progress{State: store.ApprovalStateParked},
	}
	if !prepareForResume(&j) {
		t.Fatal("parked approval job was not claimed for resume")
	}
	if j.Status != store.StatusQueued || j.HermesRunID != "" || len(j.Approvals) != 1 {
		t.Fatalf("parked resume reset lost evidence: %+v", j)
	}
	if j.Approvals[0].ID != approval.ID || j.Approvals[0].DecisionNonce != approval.DecisionNonce ||
		j.Approvals[0].State != store.ApprovalStateParked {
		t.Fatalf("parked authority evidence mutated: %+v", j.Approvals)
	}
}

func TestPrepareForResumeRejectsDuplicateClaim(t *testing.T) {
	j := store.Job{Status: store.StatusQueued, Error: "preserve"}
	if prepareForResume(&j) {
		t.Fatal("queued job was claimed for duplicate resume")
	}
	if j.Status != store.StatusQueued || j.Error != "preserve" {
		t.Fatalf("rejected resume mutated job: %+v", j)
	}
}

func TestWorkerHasNoElapsedRuntimeDeadline(t *testing.T) {
	w := &Worker{}
	if got := w.Timeout(nil); got != -1 {
		t.Fatalf("worker timeout = %s, want -1 (no timeout)", got)
	}
}

// The display name is the operator's only voice handle on a job. dispatch_job
// validated it but rename_job did not, so a model could leave a job addressable
// only by a ULID nobody speaks — and blank names additionally collide with each
// other in the exact-name lookup.
func TestJobNamesAreNormalizedAndNeverBlank(t *testing.T) {
	rejected := []string{"", "   ", "\t\n", "  "}
	for _, raw := range rejected {
		if got, err := jobName(raw); err == nil {
			t.Errorf("jobName(%q) = %q, want an error", raw, got)
		}
	}
	accepted := map[string]string{
		"CVE triage sweep":  "CVE triage sweep",
		"  padded  name  ":  "padded name",
		"multi\nline\tname": "multi line name",
		"café audit":        "café audit",
	}
	for raw, want := range accepted {
		got, err := jobName(raw)
		if err != nil {
			t.Errorf("jobName(%q) = %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("jobName(%q) = %q, want %q", raw, got, want)
		}
	}
	// Bounded by runes so a multi-byte name cannot be cut mid-character.
	long, err := jobName(strings.Repeat("é", maxJobNameRunes+50))
	if err != nil {
		t.Fatal(err)
	}
	if runes := []rune(long); len(runes) != maxJobNameRunes {
		t.Fatalf("bounded name = %d runes, want %d", len(runes), maxJobNameRunes)
	}
	if !utf8.ValidString(long) {
		t.Fatal("bounded name is not valid UTF-8")
	}
}
