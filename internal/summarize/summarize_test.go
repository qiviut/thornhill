package summarize

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"thornhill/internal/dummyopenai"
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
