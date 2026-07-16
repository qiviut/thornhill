package summarize

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"thornhill/internal/dummyopenai"
	"thornhill/internal/events"
	"thornhill/internal/store"
)

type summaryTestStore struct {
	usageSource string
	input       int64
	output      int64
}

func (*summaryTestStore) GetSummary(context.Context, string) (string, time.Time, error) {
	return "", time.Time{}, nil
}
func (*summaryTestStore) SaveSummary(context.Context, string, string) error { return nil }
func (s *summaryTestStore) AddUsage(_ context.Context, source string, in, out int64, _ float64) error {
	s.usageSource, s.input, s.output = source, in, out
	return nil
}

func TestCompleteUsesConfiguredProviderAndRecordsUsage(t *testing.T) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	token := "dummy_" + hex.EncodeToString(raw[:])
	server := httptest.NewServer(dummyopenai.New(token).Handler())
	defer server.Close()
	st := &summaryTestStore{}
	s := New(token, server.URL, "dummy-summary", st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	out, err := s.complete(context.Background(), "random-"+hex.EncodeToString(raw[8:]))
	if err != nil {
		t.Fatal(err)
	}
	if out != "deterministic dummy summary" {
		t.Fatalf("summary = %q", out)
	}
	if st.usageSource != "summary" || st.input != 1 || st.output != 1 {
		t.Fatalf("usage source=%q input=%d output=%d", st.usageSource, st.input, st.output)
	}
}

func TestLineForParkedApprovalStatesNoDecisionAndFreshAuthority(t *testing.T) {
	payload, err := json.Marshal(store.Job{DisplayName: "System audit", Status: store.StatusParkedApproval})
	if err != nil {
		t.Fatal(err)
	}
	line, keep := lineFor(events.Event{TS: time.Unix(1_700_000_000, 0), Kind: events.KindJobApprovalParked, Payload: payload})
	if !keep || !strings.Contains(line, "parked an unresolved approval") ||
		!strings.Contains(line, "released its run") || !strings.Contains(line, "fresh authority") {
		t.Fatalf("parked approval line = %q, keep=%v", line, keep)
	}
}
