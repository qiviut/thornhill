package audio

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"thornhill/internal/dummyopenai"
)

func TestSpeakUsesConfiguredProvider(t *testing.T) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	token := "dummy_" + hex.EncodeToString(raw[:])
	server := httptest.NewServer(dummyopenai.New(token).Handler())
	defer server.Close()
	tts := New(token, server.URL, "dummy-speech", "dummy-voice", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, err := tts.Speak(context.Background(), "random-"+hex.EncodeToString(raw[8:]))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 3 || string(got[:3]) != "ID3" {
		t.Fatalf("speech payload = %q", got)
	}
}
