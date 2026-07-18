package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"thornhill/internal/events"
	"thornhill/internal/store"
)

type fakeDeliveryStore struct {
	deliveries []store.PushDelivery
	delivered  []int64
	failed     []int64
	abandoned  []int64
	lastError  string
	retryAt    time.Time
	disabled   []int64
}

func (s *fakeDeliveryStore) ClaimPushDeliveries(context.Context, string, int, time.Duration) ([]store.PushDelivery, error) {
	out := s.deliveries
	s.deliveries = nil
	return out, nil
}
func (s *fakeDeliveryStore) ReleasePushClaim(context.Context, string) error {
	return nil
}
func (s *fakeDeliveryStore) MarkPushDelivered(_ context.Context, _ string, id int64) error {
	s.delivered = append(s.delivered, id)
	return nil
}
func (s *fakeDeliveryStore) MarkPushFailed(_ context.Context, _ string, id int64, retryAt time.Time, message string) error {
	s.failed = append(s.failed, id)
	s.retryAt = retryAt
	s.lastError = message
	return nil
}
func (s *fakeDeliveryStore) MarkPushAbandoned(_ context.Context, _ string, id int64, message string) error {
	s.abandoned = append(s.abandoned, id)
	s.lastError = message
	return nil
}
func (s *fakeDeliveryStore) DisablePushSubscription(_ context.Context, id int64) error {
	s.disabled = append(s.disabled, id)
	return nil
}

type fakeSender struct {
	status  int
	err     error
	payload []byte
}

func (s *fakeSender) Send(_ context.Context, payload []byte, _ store.PushDelivery) (int, error) {
	s.payload = append([]byte(nil), payload...)
	return s.status, s.err
}

func testWorker(st DeliveryStore, sender Sender) *Worker {
	return &Worker{
		Store: st, Sender: sender,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:   func() time.Time { return time.Unix(1_700_000_000, 0) },
		token: "claim-token",
	}
}

func TestDeliveryUsesPrivacySafePayloadAndAcknowledges(t *testing.T) {
	st := &fakeDeliveryStore{deliveries: []store.PushDelivery{{
		ID: 4, AttentionID: 9, Title: "Thornhill job finished",
		Body:     "A background job finished. Open Thornhill for details.",
		Endpoint: "https://push.example.test/capability-secret",
	}}}
	sender := &fakeSender{status: 201}
	testWorker(st, sender).deliver(context.Background())
	if !reflect.DeepEqual(st.delivered, []int64{4}) || len(st.failed) != 0 {
		t.Fatalf("delivered=%v failed=%v", st.delivered, st.failed)
	}
	var payload notificationPayload
	if err := json.Unmarshal(sender.payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Tag != "thornhill-attention-9" || payload.URL != "/?attention=9" {
		t.Fatalf("payload = %+v", payload)
	}
	if string(sender.payload) == "" || contains(string(sender.payload), "capability-secret") || contains(string(sender.payload), "audit") {
		t.Fatalf("private endpoint or job data leaked into notification payload: %s", sender.payload)
	}
}

func TestGoneSubscriptionIsDisabledWithoutRetry(t *testing.T) {
	st := &fakeDeliveryStore{deliveries: []store.PushDelivery{{ID: 5, SubscriptionID: 12}}}
	sender := &fakeSender{status: 410, err: errors.New("gone")}
	testWorker(st, sender).deliver(context.Background())
	if !reflect.DeepEqual(st.disabled, []int64{12}) || len(st.failed) != 0 || len(st.delivered) != 0 {
		t.Fatalf("disabled=%v failed=%v delivered=%v", st.disabled, st.failed, st.delivered)
	}
}

func TestTransientFailureUsesBoundedExponentialBackoff(t *testing.T) {
	st := &fakeDeliveryStore{deliveries: []store.PushDelivery{{ID: 6, Attempts: 3}}}
	sender := &fakeSender{status: 503, err: errors.New("unavailable")}
	w := testWorker(st, sender)
	w.deliver(context.Background())
	want := w.Now().Add(2 * time.Minute)
	if !reflect.DeepEqual(st.failed, []int64{6}) || !st.retryAt.Equal(want) {
		t.Fatalf("failed=%v retry=%s want=%s", st.failed, st.retryAt, want)
	}
	if retryDelay(99) != 16*time.Minute {
		t.Fatalf("retry cap = %s", retryDelay(99))
	}
}

func TestPermanentAndExhaustedFailuresAreAbandoned(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		attempts int
	}{
		{name: "permanent response", status: 400, attempts: 1},
		{name: "retry budget exhausted", status: 503, attempts: maxDeliveryAttempts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeDeliveryStore{deliveries: []store.PushDelivery{{ID: 7, Attempts: tc.attempts}}}
			testWorker(st, &fakeSender{status: tc.status, err: errors.New("rejected")}).deliver(context.Background())
			if !reflect.DeepEqual(st.abandoned, []int64{7}) || len(st.failed) != 0 {
				t.Fatalf("abandoned=%v failed=%v", st.abandoned, st.failed)
			}
		})
	}
}

func TestDeliveryErrorsNeverPersistEndpointCapabilities(t *testing.T) {
	const capability = "https://push.example.test/unguessable-capability"
	st := &fakeDeliveryStore{deliveries: []store.PushDelivery{{ID: 8, Attempts: 1}}}
	testWorker(st, &fakeSender{err: errors.New("Post " + capability + ": network failed")}).deliver(context.Background())
	if st.lastError == "" || strings.Contains(st.lastError, capability) || strings.Contains(st.lastError, "unguessable") {
		t.Fatalf("unsafe persisted error %q", st.lastError)
	}
}

func TestPublicPushAddressPolicy(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "100.100.100.100", "169.254.169.254", "192.0.2.1", "::1", "fc00::1", "2001:db8::1"} {
		if IsPublicPushIP(net.ParseIP(raw)) {
			t.Errorf("non-public address %s accepted", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if !IsPublicPushIP(net.ParseIP(raw)) {
			t.Errorf("public address %s rejected", raw)
		}
	}
}

func TestNoteworthyEventsMatchDurableAttentionTransitions(t *testing.T) {
	for _, kind := range []string{
		events.KindJobDone, events.KindJobFailed, events.KindJobNeedsInput,
		events.KindJobNeedsApproval, events.KindJobApprovalParked,
	} {
		if !noteworthy(kind) {
			t.Errorf("%s is not noteworthy", kind)
		}
	}
	if noteworthy(events.KindJobProgress) || noteworthy(events.KindJobQueued) {
		t.Fatal("routine progress would create notification noise")
	}
}

type blockingDeliveryStore struct {
	mu         sync.Mutex
	deliveries []store.PushDelivery
	delivered  []int64
	released   chan struct{}
	releaseOne sync.Once
}

func (s *blockingDeliveryStore) ClaimPushDeliveries(context.Context, string, int, time.Duration) ([]store.PushDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]store.PushDelivery(nil), s.deliveries...)
	s.deliveries = nil
	return out, nil
}
func (s *blockingDeliveryStore) ReleasePushClaim(context.Context, string) error {
	s.releaseOne.Do(func() { close(s.released) })
	return nil
}
func (s *blockingDeliveryStore) MarkPushDelivered(_ context.Context, _ string, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered = append(s.delivered, id)
	return nil
}
func (s *blockingDeliveryStore) MarkPushFailed(context.Context, string, int64, time.Time, string) error {
	return nil
}
func (s *blockingDeliveryStore) MarkPushAbandoned(context.Context, string, int64, string) error {
	return nil
}
func (s *blockingDeliveryStore) DisablePushSubscription(context.Context, int64) error { return nil }

type blockingSender struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   []int64
}

func (s *blockingSender) Send(ctx context.Context, _ []byte, delivery store.PushDelivery) (int, error) {
	s.mu.Lock()
	s.calls = append(s.calls, delivery.ID)
	first := len(s.calls) == 1
	s.mu.Unlock()
	if first {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return 201, nil
}

func TestLiveTransitionRemainsResponsiveDuringSlowDelivery(t *testing.T) {
	bus := events.NewBus(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	st := &blockingDeliveryStore{
		deliveries: []store.PushDelivery{{ID: 1}, {ID: 2}},
		released:   make(chan struct{}),
	}
	sender := &blockingSender{entered: make(chan struct{}), release: make(chan struct{})}
	worker := New(st, bus, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { worker.Run(ctx); close(done) }()

	select {
	case <-sender.entered:
	case <-time.After(time.Second):
		t.Fatal("first delivery did not start")
	}
	bus.Publish(events.KindSessionState, "", map[string]string{"state": "LIVE"})
	deadline := time.Now().Add(time.Second)
	for !worker.live.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !worker.live.Load() {
		t.Fatal("live transition was blocked by provider call")
	}
	select {
	case <-st.released:
	case <-time.After(time.Second):
		t.Fatal("remaining delivery claims were not released")
	}
	st.mu.Lock()
	if len(st.delivered) != 0 {
		t.Fatalf("cancelled live-transition delivery was acknowledged: %v", st.delivered)
	}
	st.mu.Unlock()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if !reflect.DeepEqual(sender.calls, []int64{1}) {
		t.Fatalf("provider calls=%v; second notification leaked into live session", sender.calls)
	}
}

func contains(value, fragment string) bool {
	for i := 0; i+len(fragment) <= len(value); i++ {
		if value[i:i+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
