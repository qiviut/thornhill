package bridge

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"thornhill/internal/events"
	"thornhill/internal/store"
)

type fuzzApprovalTransport struct{ calls atomic.Int32 }

func (r *fuzzApprovalTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"resolved":1}`)),
	}, nil
}

func FuzzApprovalDecisionIsSingleUse(f *testing.F) {
	f.Add(byte(0), true, []byte("shell\x00network"))
	f.Add(byte(2), false, []byte("shell"))
	f.Add(byte(6), true, []byte(""))
	decisions := []string{
		DecisionAllowOnce,
		DecisionAllowSession,
		DecisionAllowAlways,
		DecisionDenyOnce,
		DecisionDenySession,
		DecisionDenyAlways,
		"invalid",
	}
	f.Fuzz(func(t *testing.T, selector byte, allowPermanent bool, data []byte) {
		if len(data) > 1024 {
			data = data[:1024]
		}
		parts := bytes.Split(data, []byte{0})
		if len(parts) > 32 {
			parts = parts[:32]
		}
		patterns := make([]string, 0, len(parts))
		for _, part := range parts {
			if len(part) > 256 {
				part = part[:256]
			}
			patterns = append(patterns, string(part))
		}
		decision := decisions[int(selector)%len(decisions)]
		const jobID, runID, approvalID, nonce = "fuzz-job", "fuzz-run", "fuzz-approval", "fuzz-nonce"
		fs := &fakeStore{jobs: map[string]store.Job{
			jobID: {
				ID: jobID, DisplayName: "fuzz", Status: store.StatusNeedsApproval, HermesRunID: runID,
				Approvals: []store.Approval{{
					ID: approvalID, DecisionNonce: nonce, State: "pending",
					PatternKeys: patterns, AllowPermanent: allowPermanent,
				}},
			},
		}, permanent: map[string]bool{}, allows: map[string]bool{}}
		h := NewHermes("http://fuzz.invalid", "", "dummy", fs, events.NewBus(nil, testLog()), testLog())
		transport := &fuzzApprovalTransport{}
		h.HTTP = &http.Client{Transport: transport}
		ownRun(h, jobID, runID)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		_, err := h.DecideApproval(ctx, jobID, approvalID, nonce, decision)
		reusable := decision == DecisionAllowSession || decision == DecisionAllowAlways || decision == DecisionDenySession || decision == DecisionDenyAlways
		valid := decision != "invalid" && (!reusable || store.ApprovalPatternHash(patterns) != "") && (decision != DecisionAllowAlways || allowPermanent)
		if valid {
			if err != nil {
				t.Fatalf("valid decision %q failed: %v", decision, err)
			}
			if transport.calls.Load() != 1 {
				t.Fatalf("authority calls = %d, want 1", transport.calls.Load())
			}
			if _, replayErr := h.DecideApproval(ctx, jobID, approvalID, nonce, decision); replayErr == nil {
				t.Fatal("replayed approval succeeded")
			}
			if transport.calls.Load() != 1 {
				t.Fatalf("replay made authority call; total=%d", transport.calls.Load())
			}
			return
		}
		if err == nil {
			t.Fatalf("invalid or ineligible decision %q succeeded", decision)
		}
		if transport.calls.Load() != 0 {
			t.Fatalf("rejected decision reached authority %d times", transport.calls.Load())
		}
	})
}

type fuzzParkTransport struct {
	approvalCalls atomic.Int32
	stopCalls     atomic.Int32
}

func (r *fuzzParkTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/approval") {
		r.approvalCalls.Add(1)
	}
	if strings.HasSuffix(req.URL.Path, "/stop") {
		r.stopCalls.Add(1)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}, nil
}

func FuzzParkedApprovalNeverInvokesAuthority(f *testing.F) {
	f.Add(true, true, []byte("operator silent"))
	f.Add(false, true, []byte("restart"))
	f.Add(true, false, []byte("resource reclamation"))
	f.Fuzz(func(t *testing.T, matchID, matchNonce bool, reasonBytes []byte) {
		if len(reasonBytes) > 1024 {
			reasonBytes = reasonBytes[:1024]
		}
		const jobID, runID, approvalID, nonce = "park-fuzz-job", "park-fuzz-run", "park-fuzz-approval", "park-fuzz-nonce"
		candidateID, candidateNonce := approvalID, nonce
		if !matchID {
			candidateID += "-stale"
		}
		if !matchNonce {
			candidateNonce += "-stale"
		}
		fs := &fakeStore{jobs: map[string]store.Job{jobID: {
			ID: jobID, Status: store.StatusNeedsApproval, HermesRunID: runID,
			Approvals: []store.Approval{{ID: approvalID, DecisionNonce: nonce, State: store.ApprovalStatePending}},
		}}}
		h := NewHermes("http://fuzz.invalid", "", "dummy", fs, events.NewBus(nil, testLog()), testLog())
		transport := &fuzzParkTransport{}
		h.HTTP = &http.Client{Transport: transport}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		_, err := h.parkApproval(ctx, jobID, runID, candidateID, candidateNonce, string(reasonBytes))
		valid := matchID && matchNonce
		if valid && err != nil {
			t.Fatalf("valid parking failed: %v", err)
		}
		if !valid && err == nil {
			t.Fatal("mismatched parking token succeeded")
		}
		if transport.approvalCalls.Load() != 0 {
			t.Fatalf("parking reached authority endpoint %d times", transport.approvalCalls.Load())
		}
		wantStops := int32(0)
		if valid {
			wantStops = 1
			parked, resolveErr := fs.ResolveJob(ctx, jobID)
			if resolveErr != nil || parked.Status != store.StatusParkedApproval ||
				len(parked.Approvals) != 1 || parked.Approvals[0].State != store.ApprovalStateParked {
				t.Fatalf("parked state = %+v, err=%v", parked, resolveErr)
			}
			if _, replayErr := h.DecideApproval(ctx, jobID, approvalID, nonce, DecisionAllowOnce); replayErr == nil {
				t.Fatal("parked authority token was replayable")
			}
		}
		if transport.stopCalls.Load() != wantStops {
			t.Fatalf("stop calls = %d, want %d", transport.stopCalls.Load(), wantStops)
		}
	})
}
