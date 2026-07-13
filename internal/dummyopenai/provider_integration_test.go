//go:build integration

package dummyopenai_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"thornhill/internal/dummyopenai"
	"thornhill/internal/openairt"
)

func randomHex(t *testing.T, bytes int) string {
	t.Helper()
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

func TestProviderProcessConformance(t *testing.T) {
	t.Parallel()
	token := "dummy_" + randomHex(t, 32)
	binary := filepath.Join(t.TempDir(), "dummy-openai")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, "../../cmd/dummy-openai")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build dummy provider: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--listen", "127.0.0.1:0", "--token", token)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case err := <-waited:
			if err != nil && ctx.Err() == nil {
				t.Errorf("dummy provider exit: %v; stderr=%s", err, stderr.String())
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			t.Error("dummy provider did not terminate after SIGTERM")
		}
	}()

	lineCh := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		lineCh <- strings.TrimSpace(line)
	}()
	var endpointLine string
	select {
	case endpointLine = <-lineCh:
	case <-time.After(10 * time.Second):
		t.Fatalf("provider readiness timeout; stderr=%s", stderr.String())
	}
	baseURL, wsURL, err := dummyopenai.ParseEndpoint(endpointLine)
	if err != nil {
		t.Fatalf("parse endpoint %q: %v", endpointLine, err)
	}
	wsURL += "?model=dummy-" + randomHex(t, 12)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(baseURL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}

	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	_ = writer.WriteField("sdp", "v=0\r\ns=random-"+randomHex(t, 12)+"\r\n")
	_ = writer.WriteField("session", `{"type":"realtime","model":"dummy"}`)
	_ = writer.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/realtime/calls", &form)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || !bytes.Contains(answer, []byte("dummy-openai")) {
		t.Fatalf("create call status=%d answer=%q", resp.StatusCode, answer)
	}
	parts := strings.Split(strings.TrimRight(resp.Header.Get("Location"), "/"), "/")
	callID := parts[len(parts)-1]
	if !strings.HasPrefix(callID, "rtc_dummy_") {
		t.Fatalf("call ID = %q", callID)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	realtime, err := openairt.DialURL(ctx, token, callID, wsURL, log)
	if err != nil {
		t.Fatal(err)
	}
	defer realtime.Close()
	if ev, err := realtime.Read(ctx); err != nil || ev.Type != openairt.EvSessionCreated {
		t.Fatalf("first event=%q err=%v", ev.Type, err)
	}
	if err := realtime.SessionUpdate(ctx, "deterministic test", nil, "dummy-transcribe"); err != nil {
		t.Fatal(err)
	}
	if ev, err := realtime.Read(ctx); err != nil || ev.Type != openairt.EvSessionUpdated {
		t.Fatalf("session event=%q err=%v", ev.Type, err)
	}
	if err := realtime.InjectMessage(ctx, "user", "random-"+randomHex(t, 16)); err != nil {
		t.Fatal(err)
	}
	if err := realtime.CreateResponse(ctx, "provider_conformance_"+randomHex(t, 16)); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for len(seen) < 3 {
		ev, readErr := realtime.Read(ctx)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if ev.Type == openairt.EvResponseCreated || ev.Type == openairt.EvOutputTranscriptDone || ev.Type == openairt.EvResponseDone {
			seen[ev.Type] = true
		}
	}

	for path, body := range map[string]string{
		"/v1/audio/speech":     `{"model":"dummy","voice":"dummy","input":"` + randomHex(t, 10) + `"}`,
		"/v1/chat/completions": `{"model":"dummy","messages":[{"role":"user","content":"` + randomHex(t, 10) + `"}]}`,
	} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, requestErr := client.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		var payload any
		if path == "/v1/chat/completions" {
			requestErr = json.NewDecoder(resp.Body).Decode(&payload)
		} else {
			_, requestErr = io.ReadAll(resp.Body)
		}
		resp.Body.Close()
		if requestErr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d err=%v", path, resp.StatusCode, requestErr)
		}
	}
}
