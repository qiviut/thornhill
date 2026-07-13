package openairt

import (
	"encoding/json"
	"testing"
)

func TestExtractFuncCallsReturnsOnlyCompletedCallsFromCompletedResponse(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{
		"response": {
			"status": "completed",
			"output": [
				{"type":"function_call","status":"completed","call_id":"call-ok","name":"job_status","arguments":"{\"job\":\"audit\"}"},
				{"type":"function_call","status":"in_progress","call_id":"call-partial","name":"cancel_job","arguments":""},
				{"type":"message","status":"completed"}
			]
		}
	}`)
	calls := ExtractFuncCalls(raw)
	if len(calls) != 1 {
		t.Fatalf("calls = %#v, want one completed call", calls)
	}
	if calls[0].CallID != "call-ok" || calls[0].Name != "job_status" || calls[0].Arguments != `{"job":"audit"}` {
		t.Fatalf("call = %#v", calls[0])
	}
}

func TestExtractFuncCallsRejectsCancelledResponse(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{
		"response": {
			"status": "cancelled",
			"output": [
				{"type":"function_call","status":"completed","call_id":"call-unsafe","name":"cancel_job","arguments":"{\"job\":\"audit\"}"}
			]
		}
	}`)
	if calls := ExtractFuncCalls(raw); len(calls) != 0 {
		t.Fatalf("cancelled response produced executable calls: %#v", calls)
	}
}

func TestExtractErrorEventIDSupportsNestedAndTopLevelShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{
			name: "ga nested error",
			raw:  json.RawMessage(`{"type":"error","error":{"code":"invalid_value","event_id":"evt-nested"}}`),
			want: "evt-nested",
		},
		{
			name: "provider compatible top level",
			raw:  json.RawMessage(`{"type":"error","event_id":"evt-top","error":{"code":"invalid_value"}}`),
			want: "evt-top",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractErrorEventID(tc.raw); got != tc.want {
				t.Fatalf("ExtractErrorEventID() = %q, want %q", got, tc.want)
			}
		})
	}
}
