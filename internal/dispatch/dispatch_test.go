package dispatch

import (
	"testing"
	"time"

	"thornhill/internal/store"
)

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
