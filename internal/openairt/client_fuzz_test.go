package openairt

import (
	"context"
	"encoding/json"
	"testing"
)

func TestDialURLRejectsUnsafeEndpointsBeforeNetwork(t *testing.T) {
	for _, endpoint := range []string{
		"http://models.example.test/v1/realtime",
		"ws://models.example.test/v1/realtime",
		"wss://user:pass@models.example.test/v1/realtime",
		"wss://models.example.test/v1/realtime#fragment",
		"not-a-url",
	} {
		if _, err := DialURL(context.Background(), "secret", "rtc_test", endpoint, nil); err == nil {
			t.Fatalf("DialURL accepted unsafe endpoint %q", endpoint)
		}
	}
}

func FuzzRealtimeEventExtractors(f *testing.F) {
	f.Add([]byte(`{"type":"response.done","response":{"output":[{"type":"function_call","call_id":"c","name":"job_status","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":2}}}`))
	f.Add([]byte(`{"type":"error","error":{"type":"invalid_request_error","code":"bad","message":"no"}}`))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 16<<10 {
			raw = raw[:16<<10]
		}
		message := json.RawMessage(raw)
		calls := ExtractFuncCalls(message)
		_ = ExtractUsage(message)
		_ = ExtractTranscript(message)
		_ = ExtractError(message)
		for _, call := range calls {
			if !json.Valid([]byte(call.Arguments)) && call.Arguments != "" {
				// The wire field is itself an opaque JSON string. Extraction must
				// preserve it byte-for-byte rather than silently normalizing it.
				var body struct {
					Response struct {
						Output []struct {
							Type      string `json:"type"`
							CallID    string `json:"call_id"`
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"output"`
					} `json:"response"`
				}
				if json.Unmarshal(raw, &body) == nil {
					found := false
					for _, item := range body.Response.Output {
						if item.Type == "function_call" && item.CallID == call.CallID && item.Name == call.Name && item.Arguments == call.Arguments {
							found = true
						}
					}
					if !found {
						t.Fatal("function-call extractor synthesized arguments")
					}
				}
			}
		}
	})
}
