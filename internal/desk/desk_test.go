package desk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"thornhill/internal/events"
	"thornhill/internal/openairt"
	"thornhill/internal/store"
)

func TestToolSchemasAlwaysEncodeRequiredAsArray(t *testing.T) {
	t.Parallel()

	for _, tool := range toolset() {
		var schema struct {
			Required json.RawMessage `json:"required"`
		}
		if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
			t.Fatalf("%s parameters: %v", tool.Name, err)
		}
		var required []string
		if err := json.Unmarshal(schema.Required, &required); err != nil {
			t.Errorf("%s required must be an array: %s", tool.Name, schema.Required)
		}
	}
}

func TestResolveApprovalToolDefaultsToOnceAndOffersSaferAlternative(t *testing.T) {
	t.Parallel()
	var got []string
	for _, tool := range toolset() {
		if tool.Name != "resolve_approval" {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		got = schema.Properties["decision"].Enum
	}
	want := []string{"allow_once", "deny_once", "use_safer_alternative", "allow_session", "deny_session", "allow_always", "deny_always"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decision enum = %v, want %v", got, want)
	}
}

func TestApprovalAnnouncementIsProactiveAndConversational(t *testing.T) {
	t.Parallel()
	job := store.Job{
		DisplayName: "System audit",
		Approvals: []store.Approval{{
			ID: "approval-secret-id", Command: "systemctl restart demo",
			Description: "restart the demo service", RequestedAt: time.Now(),
		}},
	}
	payload, _ := json.Marshal(job)
	d := &Desk{}
	inj, ok := d.announcementFor(events.Event{Kind: events.KindJobNeedsApproval, Payload: payload})
	if !ok || !inj.respond || !inj.isQuestion || inj.role != "system" {
		t.Fatalf("announcement flags: %+v, ok=%v", inj, ok)
	}
	for _, want := range []string{"System audit", "job_status", "untrusted data", "safer alternative", "only if the operator asks", "Do not resolve"} {
		if !strings.Contains(inj.text, want) {
			t.Errorf("announcement missing %q: %s", want, inj.text)
		}
	}
}

func TestParkedApprovalAnnouncementCannotResolveStaleAuthority(t *testing.T) {
	t.Parallel()
	job := store.Job{DisplayName: "System audit", Status: store.StatusParkedApproval,
		Approvals: []store.Approval{{ID: "stale-id", DecisionNonce: "stale-nonce", State: store.ApprovalStateParked}}}
	payload, _ := json.Marshal(job)
	d := &Desk{}
	inj, ok := d.announcementFor(events.Event{Kind: events.KindJobApprovalParked, Payload: payload})
	if !ok || !inj.respond || inj.isQuestion || inj.role != "system" {
		t.Fatalf("announcement flags: %+v, ok=%v", inj, ok)
	}
	for _, want := range []string{"without allowing or denying", "resume_job", "fresh authority request", "Do not call resolve_approval"} {
		if !strings.Contains(inj.text, want) {
			t.Errorf("parked announcement missing %q: %s", want, inj.text)
		}
	}

	view := full(job)
	if _, ok := view["pending_approval"]; ok {
		t.Fatalf("parked request exposed as pending: %#v", view)
	}
	if _, ok := view["approval_choices"]; ok {
		t.Fatalf("parked request exposed authority choices: %#v", view)
	}
	if _, ok := view["parked_approval"]; !ok {
		t.Fatalf("parked evidence missing: %#v", view)
	}
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), "stale-id") || strings.Contains(string(encoded), "stale-nonce") {
		t.Fatalf("parked authority token leaked through desk view: %s", encoded)
	}
}

type recordingRealtime struct {
	outputCallIDs  []string
	outputBodies   []string
	createEventIDs []string
	creates        int
	injectErr      error
	createErr      error
}

func (*recordingRealtime) Close() {}
func (*recordingRealtime) Read(context.Context) (openairt.ServerEvent, error) {
	return openairt.ServerEvent{}, io.EOF
}
func (*recordingRealtime) SessionUpdate(context.Context, string, []openairt.Tool, string) error {
	return nil
}
func (r *recordingRealtime) InjectMessage(context.Context, string, string) error {
	return r.injectErr
}
func (r *recordingRealtime) FunctionOutput(_ context.Context, callID, output string) error {
	r.outputCallIDs = append(r.outputCallIDs, callID)
	r.outputBodies = append(r.outputBodies, output)
	return nil
}
func (r *recordingRealtime) CreateResponse(_ context.Context, eventID string) error {
	r.creates++
	r.createEventIDs = append(r.createEventIDs, eventID)
	return r.createErr
}
func (r *recordingRealtime) CancelResponse(context.Context) error { return nil }

type attentionTestStore struct {
	claimed      []store.Attention
	acknowledged []int64
	released     bool
	summary      string
	summarySaved int
}

func (s *attentionTestStore) GetSummary(context.Context, string) (string, time.Time, error) {
	return s.summary, time.Unix(1_700_000_000, 0), nil
}
func (s *attentionTestStore) SaveSummary(context.Context, string, string) error {
	s.summarySaved++
	return nil
}
func (*attentionTestStore) AddUsage(context.Context, string, int64, int64, float64) error {
	return nil
}
func (s *attentionTestStore) ClaimPendingAttention(context.Context, string, int, time.Duration) ([]store.Attention, error) {
	items := s.claimed
	s.claimed = nil
	return items, nil
}
func (s *attentionTestStore) ReleaseAttentionClaim(context.Context, string) error {
	s.released = true
	return nil
}
func (s *attentionTestStore) MarkAttentionSpoken(_ context.Context, _ string, ids []int64) (int64, error) {
	s.acknowledged = append(s.acknowledged, ids...)
	return int64(len(ids)), nil
}

func testDesk(t *testing.T, client realtimeClient) *Desk {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := New(Deps{
		Bus:       events.NewBus(nil, log),
		Log:       log,
		FSMConfig: DefaultFSMConfig(time.Minute, time.Hour, time.Hour),
	})
	d.client = client
	d.fsm.CallStarted(time.Now())
	return d
}

func TestDurableAttentionAcknowledgesOnlyAfterCompletedOutputAudio(t *testing.T) {
	client := &recordingRealtime{}
	d := testDesk(t, client)
	st := &attentionTestStore{}
	d.Store = st
	inj, ok := d.attentionBriefing([]store.Attention{{ID: 7, SpeechText: "Job quoted-data finished."}})
	if !ok || !inj.respond || !reflect.DeepEqual(inj.attention, []int64{7}) ||
		!strings.Contains(inj.text, "untrusted job data") {
		t.Fatalf("briefing injection = %+v, ok=%v", inj, ok)
	}
	if err := d.doInject(context.Background(), inj); err != nil {
		t.Fatal(err)
	}
	pendingQuestion := false
	requestID := client.createEventIDs[0]
	created := json.RawMessage(fmt.Sprintf(`{"type":"response.created","response":{"id":"resp-brief","metadata":{"thornhill_request_id":%q}}}`, requestID))
	if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvResponseCreated, Raw: created}, &pendingQuestion); err != nil {
		t.Fatal(err)
	}
	audio := json.RawMessage(`{"response_id":"resp-brief"}`)
	if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvOutputAudioStarted, Raw: audio}, &pendingQuestion); err != nil {
		t.Fatal(err)
	}
	done := json.RawMessage(`{"type":"response.done","response":{"id":"resp-brief","status":"completed","output":[]}}`)
	if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvResponseDone, Raw: done}, &pendingQuestion); err != nil {
		t.Fatal(err)
	}
	if len(st.acknowledged) != 0 {
		t.Fatalf("response completion acknowledged before output drain: %v", st.acknowledged)
	}
	if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvOutputAudioStopped, Raw: audio}, &pendingQuestion); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(st.acknowledged, []int64{7}) || len(d.responseAttention) != 0 {
		t.Fatalf("acknowledged=%v pending=%v", st.acknowledged, d.responseAttention)
	}
}

func TestAttentionTransportFailuresRequeueBriefing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		injectErr error
		createErr error
	}{
		{name: "message injection", injectErr: fmt.Errorf("inject failed")},
		{name: "response creation", createErr: fmt.Errorf("create failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &recordingRealtime{injectErr: tc.injectErr, createErr: tc.createErr}
			d := testDesk(t, client)
			inj, _ := d.attentionBriefing([]store.Attention{{ID: 10, SpeechText: "pending"}})
			if err := d.doInject(context.Background(), inj); err == nil {
				t.Fatal("transport failure unexpectedly succeeded")
			}
			retry, ok := d.nextInjection()
			if !ok || retry.text != inj.text || !reflect.DeepEqual(retry.attention, []int64{10}) {
				t.Fatalf("retry=%+v ok=%v", retry, ok)
			}
		})
	}
}

func TestStaleResponseCallbacksCannotAcknowledgeAttention(t *testing.T) {
	client := &recordingRealtime{}
	d := testDesk(t, client)
	st := &attentionTestStore{}
	d.Store = st
	inj, _ := d.attentionBriefing([]store.Attention{{ID: 8, SpeechText: "pending"}})
	if err := d.doInject(context.Background(), inj); err != nil {
		t.Fatal(err)
	}
	pendingQuestion := false
	created := json.RawMessage(fmt.Sprintf(`{"response":{"id":"resp-current","metadata":{"thornhill_request_id":%q}}}`, client.createEventIDs[0]))
	if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvResponseCreated, Raw: created}, &pendingQuestion); err != nil {
		t.Fatal(err)
	}
	staleAudio := json.RawMessage(`{"response_id":"resp-stale"}`)
	staleDone := json.RawMessage(`{"response":{"id":"resp-stale","status":"completed"}}`)
	for _, event := range []openairt.ServerEvent{
		{Type: openairt.EvOutputAudioStarted, Raw: staleAudio},
		{Type: openairt.EvResponseDone, Raw: staleDone},
		{Type: openairt.EvOutputAudioStopped, Raw: staleAudio},
	} {
		if err := d.handleServer(context.Background(), event, &pendingQuestion); err != nil {
			t.Fatal(err)
		}
	}
	if len(st.acknowledged) != 0 {
		t.Fatalf("stale response acknowledged attention: %v", st.acknowledged)
	}
}

func TestInterruptedOrInaudibleAttentionRequeuesUntilHeard(t *testing.T) {
	for _, tc := range []struct {
		name   string
		finish func(*testing.T, *Desk, *bool)
	}{
		{name: "cleared audio", finish: func(t *testing.T, d *Desk, pendingQuestion *bool) {
			audio := json.RawMessage(`{"response_id":"resp-pending"}`)
			if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvOutputAudioStarted, Raw: audio}, pendingQuestion); err != nil {
				t.Fatal(err)
			}
			if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvOutputAudioCleared, Raw: audio}, pendingQuestion); err != nil {
				t.Fatal(err)
			}
			done := json.RawMessage(`{"response":{"id":"resp-pending","status":"completed","output":[]}}`)
			if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvResponseDone, Raw: done}, pendingQuestion); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "text only", finish: func(t *testing.T, d *Desk, pendingQuestion *bool) {
			done := json.RawMessage(`{"response":{"id":"resp-pending","status":"completed","output":[]}}`)
			if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvResponseDone, Raw: done}, pendingQuestion); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "interrupted", finish: func(t *testing.T, d *Desk, pendingQuestion *bool) {
			done := json.RawMessage(`{"response":{"id":"resp-pending","status":"cancelled","output":[]}}`)
			if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvResponseDone, Raw: done}, pendingQuestion); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &recordingRealtime{}
			d := testDesk(t, client)
			st := &attentionTestStore{}
			d.Store = st
			inj, _ := d.attentionBriefing([]store.Attention{{ID: 9, SpeechText: "pending"}})
			if err := d.doInject(context.Background(), inj); err != nil {
				t.Fatal(err)
			}
			pendingQuestion := false
			created := json.RawMessage(fmt.Sprintf(`{"response":{"id":"resp-pending","metadata":{"thornhill_request_id":%q}}}`, client.createEventIDs[0]))
			if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvResponseCreated, Raw: created}, &pendingQuestion); err != nil {
				t.Fatal(err)
			}
			tc.finish(t, d, &pendingQuestion)
			if len(st.acknowledged) != 0 {
				t.Fatalf("inaudible briefing acknowledged: %v", st.acknowledged)
			}

			retry, ok := d.nextInjection()
			if !ok || retry.text != inj.text || !reflect.DeepEqual(retry.attention, []int64{9}) {
				t.Fatalf("retry=%+v ok=%v", retry, ok)
			}
			if err := d.doInject(context.Background(), retry); err != nil {
				t.Fatal(err)
			}
			retryRequestID := client.createEventIDs[len(client.createEventIDs)-1]
			retryCreated := json.RawMessage(fmt.Sprintf(`{"response":{"id":"resp-retry","metadata":{"thornhill_request_id":%q}}}`, retryRequestID))
			if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvResponseCreated, Raw: retryCreated}, &pendingQuestion); err != nil {
				t.Fatal(err)
			}
			retryAudio := json.RawMessage(`{"response_id":"resp-retry"}`)
			if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvOutputAudioStarted, Raw: retryAudio}, &pendingQuestion); err != nil {
				t.Fatal(err)
			}
			retryDone := json.RawMessage(`{"response":{"id":"resp-retry","status":"completed","output":[]}}`)
			if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvResponseDone, Raw: retryDone}, &pendingQuestion); err != nil {
				t.Fatal(err)
			}
			if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvOutputAudioStopped, Raw: retryAudio}, &pendingQuestion); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(st.acknowledged, []int64{9}) || len(d.responseAttention) != 0 {
				t.Fatalf("acknowledged=%v pending=%v", st.acknowledged, d.responseAttention)
			}
		})
	}
}

func TestBuildingInstructionsDoesNotConsumeDebrief(t *testing.T) {
	d := testDesk(t, &recordingRealtime{})
	st := &attentionTestStore{summary: "a durable completion"}
	d.Store = st
	d.Dispatcher = &emptyDispatcher{}
	instructions := d.buildInstructions(context.Background())
	if !strings.Contains(instructions, "a durable completion") || st.summarySaved != 0 {
		t.Fatalf("instructions=%q summary saves=%d", instructions, st.summarySaved)
	}
}

type emptyDispatcher struct{ Dispatcher }

func (*emptyDispatcher) Active(context.Context) ([]store.Job, error) { return nil, nil }

func TestResponseDoneBatchesToolOutputsBeforeSingleContinuation(t *testing.T) {
	t.Parallel()
	client := &recordingRealtime{}
	d := testDesk(t, client)
	pendingQuestion := false
	raw := json.RawMessage(`{"type":"response.done","response":{"status":"completed","output":[{"type":"function_call","status":"completed","call_id":"call-1","name":"unknown_a","arguments":"{}"},{"type":"function_call","status":"completed","call_id":"call-2","name":"unknown_b","arguments":"{}"}]}}`)
	if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvResponseDone, Raw: raw}, &pendingQuestion); err != nil {
		t.Fatal(err)
	}
	batch := <-d.toolResults
	if err := d.finishToolBatch(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if want := []string{"call-1", "call-2"}; !reflect.DeepEqual(client.outputCallIDs, want) {
		t.Fatalf("function output call ids = %v, want %v", client.outputCallIDs, want)
	}
	if len(client.outputBodies) != 2 {
		t.Fatalf("function output bodies = %d, want 2", len(client.outputBodies))
	}
	for i, body := range client.outputBodies {
		var envelope struct {
			Schema string          `json:"schema"`
			Tool   string          `json:"tool"`
			CallID string          `json:"call_id"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatalf("output %d is not structured JSON: %v: %s", i, err, body)
		}
		if envelope.Schema != "thornhill.function_output.v1" || envelope.CallID != client.outputCallIDs[i] || envelope.Tool != fmt.Sprintf("unknown_%c", 'a'+rune(i)) {
			t.Fatalf("output %d envelope = %+v", i, envelope)
		}
		var result toolResult
		if err := json.Unmarshal(envelope.Result, &result); err != nil || result.OK || !strings.Contains(result.Message, "unknown tool") {
			t.Fatalf("output %d result = %+v, err=%v", i, result, err)
		}
	}
	if client.creates != 1 {
		t.Fatalf("response.create count = %d, want 1", client.creates)
	}
	if !d.fsm.Busy() {
		t.Fatal("continuation must be marked in-flight before response.created")
	}
}

type blockingDispatcher struct {
	Dispatcher
	started chan struct{}
	release chan struct{}
}

func (b *blockingDispatcher) Active(ctx context.Context) ([]store.Job, error) {
	close(b.started)
	select {
	case <-b.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestSpeechEventsRemainObservableWhileToolBatchRuns(t *testing.T) {
	t.Parallel()
	client := &recordingRealtime{}
	d := testDesk(t, client)
	blocker := &blockingDispatcher{started: make(chan struct{}), release: make(chan struct{})}
	d.Dispatcher = blocker
	pendingQuestion := false
	raw := json.RawMessage(`{"type":"response.done","response":{"status":"completed","output":[{"type":"function_call","status":"completed","call_id":"call-status","name":"job_status","arguments":"{}"}]}}`)
	if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvResponseDone, Raw: raw}, &pendingQuestion); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocker.started:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}

	if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvSpeechStarted}, &pendingQuestion); err != nil {
		t.Fatal(err)
	}
	close(blocker.release)
	var batch toolBatch
	select {
	case batch = <-d.toolResults:
	case <-time.After(time.Second):
		t.Fatal("tool batch did not finish")
	}
	if err := d.finishToolBatch(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if client.creates != 0 || !d.pendingContinuation {
		t.Fatalf("continuation created over active speech: creates=%d pending=%v", client.creates, d.pendingContinuation)
	}

	if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvSpeechStopped}, &pendingQuestion); err != nil {
		t.Fatal(err)
	}
	if client.creates != 0 {
		t.Fatalf("speech_stopped raced response.create before input commit: creates=%d", client.creates)
	}
	if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvInputAudioCommitted}, &pendingQuestion); err != nil {
		t.Fatal(err)
	}
	if client.creates != 1 {
		t.Fatalf("committed user turn did not release one serialized continuation: creates=%d", client.creates)
	}
}

func TestDuplicateActiveResponseErrorDoesNotQueueAudibleLoop(t *testing.T) {
	t.Parallel()
	client := &recordingRealtime{}
	d := testDesk(t, client)
	pendingQuestion := false
	raw := json.RawMessage(`{"type":"error","error":{"type":"invalid_request_error","code":"conversation_already_has_active_response","message":"already active"}}`)
	if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvError, Raw: raw}, &pendingQuestion); err != nil {
		t.Fatal(err)
	}
	if len(d.inject) != 0 {
		t.Fatal("duplicate-active error must not queue another spoken response")
	}
	if !d.fsm.Busy() {
		t.Fatal("known active response must remain represented as busy")
	}
}

type parallelDispatcher struct {
	Dispatcher
	started chan string
	release chan struct{}
}

func (p *parallelDispatcher) Dispatch(ctx context.Context, name, task string) (store.Job, error) {
	select {
	case p.started <- name:
	case <-ctx.Done():
		return store.Job{}, ctx.Err()
	}
	select {
	case <-p.release:
		return store.Job{ID: name, DisplayName: name}, nil
	case <-ctx.Done():
		return store.Job{}, ctx.Err()
	}
}

func TestToolBatchExecutesConcurrentlyAndPublishesInResponseOrder(t *testing.T) {
	client := &recordingRealtime{}
	d := testDesk(t, client)
	dispatcher := &parallelDispatcher{started: make(chan string, 2), release: make(chan struct{})}
	d.Dispatcher = dispatcher
	calls := []openairt.FuncCall{
		{CallID: "call-one", Name: "dispatch_job", Arguments: `{"name":"one","task":"first"}`},
		{CallID: "call-two", Name: "dispatch_job", Arguments: `{"name":"two","task":"second"}`},
	}
	d.startToolBatch(context.Background(), calls)
	for i := 0; i < 2; i++ {
		select {
		case <-dispatcher.started:
		case <-time.After(time.Second):
			t.Fatal("both tool calls did not start concurrently")
		}
	}
	close(dispatcher.release)
	batch := <-d.toolResults
	if err := d.finishToolBatch(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if want := []string{"call-one", "call-two"}; !reflect.DeepEqual(client.outputCallIDs, want) {
		t.Fatalf("ordered output ids = %v, want %v", client.outputCallIDs, want)
	}
}

func TestParkingRejectsNewContinuationResponses(t *testing.T) {
	client := &recordingRealtime{}
	d := testDesk(t, client)
	d.pendingContinuation = true
	d.RequestPark()
	if err := d.maybeCreateResponse(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.creates != 0 || !d.pendingContinuation {
		t.Fatalf("Park admitted a new response: creates=%d pending=%v", client.creates, d.pendingContinuation)
	}
}

type blockingCreateRealtime struct {
	recordingRealtime
	entered chan struct{}
	release chan struct{}
}

func (r *blockingCreateRealtime) CreateResponse(ctx context.Context, eventID string) error {
	r.creates++
	r.createEventIDs = append(r.createEventIDs, eventID)
	close(r.entered)
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestParkSerializesWithResponseCreateAdmission(t *testing.T) {
	client := &blockingCreateRealtime{entered: make(chan struct{}), release: make(chan struct{})}
	d := testDesk(t, client)
	responseDone := make(chan error, 1)
	go func() { responseDone <- d.requestResponse(context.Background()) }()
	<-client.entered

	parkDone := make(chan struct{})
	go func() {
		d.RequestPark()
		close(parkDone)
	}()
	select {
	case <-parkDone:
		t.Fatal("Park transitioned while response.create was still being written")
	case <-time.After(25 * time.Millisecond):
	}
	if got := d.fsm.State(); got != StateLive {
		t.Fatalf("state during response.create = %s, want LIVE", got)
	}

	close(client.release)
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-parkDone:
	case <-time.After(time.Second):
		t.Fatal("Park did not resume after response.create completed")
	}
	if got := d.fsm.State(); got != StateParking {
		t.Fatalf("state after serialized Park = %s, want PARKING", got)
	}
}

func TestResponseCreateErrorsAreCorrelatedByEventID(t *testing.T) {
	client := &recordingRealtime{}
	d := testDesk(t, client)
	if err := d.requestResponse(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.createEventIDs) != 1 || client.createEventIDs[0] == "" {
		t.Fatalf("response.create event IDs = %v", client.createEventIDs)
	}
	createEventID := client.createEventIDs[0]
	pendingQuestion := false

	unrelated := json.RawMessage(`{"type":"error","error":{"type":"invalid_request_error","code":"invalid_value","message":"unrelated session update rejected","event_id":"other-event"}}`)
	if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvError, Raw: unrelated}, &pendingQuestion); err != nil {
		t.Fatal(err)
	}
	if !d.fsm.Busy() || d.responseCreateEventID != createEventID || d.pendingContinuation {
		t.Fatalf("unrelated error mutated response state: busy=%v pendingID=%q continuation=%v", d.fsm.Busy(), d.responseCreateEventID, d.pendingContinuation)
	}

	matched := json.RawMessage(fmt.Sprintf(`{"type":"error","error":{"type":"invalid_request_error","code":"invalid_request","message":"response rejected","event_id":%q}}`, createEventID))
	if err := d.handleServer(context.Background(), openairt.ServerEvent{Type: openairt.EvError, Raw: matched}, &pendingQuestion); err != nil {
		t.Fatal(err)
	}
	if d.fsm.Busy() || d.responseCreateEventID != "" {
		t.Fatal("matched response.create rejection left the Desk permanently busy")
	}
	if !d.pendingContinuation {
		t.Fatal("matched response.create rejection did not preserve a retry opportunity")
	}
}
