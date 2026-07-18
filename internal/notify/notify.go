// Package notify delivers durable operator-attention obligations through Web
// Push. Push is an optional presentation adapter: failures never mutate job
// authority or mark a spoken briefing as consumed.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"thornhill/internal/events"
	"thornhill/internal/store"
)

const (
	claimLease          = time.Minute
	batchSize           = 20
	maxDeliveryAttempts = 6
)

var errVoiceBecameLive = errors.New("voice session became live")

type DeliveryStore interface {
	ClaimPushDeliveries(ctx context.Context, token string, limit int, lease time.Duration) ([]store.PushDelivery, error)
	ReleasePushClaim(ctx context.Context, token string) error
	MarkPushDelivered(ctx context.Context, token string, id int64) error
	MarkPushFailed(ctx context.Context, token string, id int64, retryAt time.Time, message string) error
	MarkPushAbandoned(ctx context.Context, token string, id int64, message string) error
	DisablePushSubscription(ctx context.Context, subscriptionID int64) error
}

type Sender interface {
	Send(ctx context.Context, payload []byte, delivery store.PushDelivery) (status int, err error)
}

type WebPushSender struct {
	PublicKey  string
	PrivateKey string
	Subject    string
	HTTP       *http.Client
}

var (
	defaultPushHTTPClient = newPushHTTPClient()
	nonPublicPushNetworks = []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("fec0::/10"),
	}
)

// IsPublicPushIP rejects loopback, tailnet, private, link-local, documentation,
// benchmark, and other non-public destinations. Push endpoints are remotely
// supplied URLs, so the sender must not become an internal-network probe.
func IsPublicPushIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, blocked := range nonPublicPushNetworks {
		if blocked.Contains(addr) {
			return false
		}
	}
	return true
}

func newPushHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Resolve and dial the endpoint directly. An environment proxy would move
	// resolution outside this process and bypass the public-address check.
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid push destination: %w", err)
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, errors.New("push destination lookup failed")
		}
		if len(ips) == 0 {
			return nil, errors.New("push destination has no addresses")
		}
		for _, ip := range ips {
			if !IsPublicPushIP(ip) {
				return nil, errors.New("push destination resolved to a non-public address")
			}
		}
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, errors.New("push destination connection failed")
	}
	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (s *WebPushSender) Send(ctx context.Context, payload []byte, delivery store.PushDelivery) (int, error) {
	client := s.HTTP
	if client == nil {
		client = defaultPushHTTPClient
	}
	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: delivery.Endpoint,
		Keys: webpush.Keys{
			P256dh: delivery.P256DH,
			Auth:   delivery.Auth,
		},
	}, &webpush.Options{
		HTTPClient:      client,
		Subscriber:      s.Subject,
		VAPIDPublicKey:  s.PublicKey,
		VAPIDPrivateKey: s.PrivateKey,
		TTL:             int((24 * time.Hour).Seconds()),
		Urgency:         webpush.UrgencyNormal,
	})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("push endpoint returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

type Worker struct {
	Store  DeliveryStore
	Bus    *events.Bus
	Sender Sender
	Log    *slog.Logger
	Now    func() time.Time
	token  string
	live   atomic.Bool
	sendMu sync.Mutex
	active *activeSend
}

type activeSend struct {
	cancel context.CancelFunc
}

func New(st DeliveryStore, bus *events.Bus, sender Sender, log *slog.Logger) *Worker {
	return &Worker{Store: st, Bus: bus, Sender: sender, Log: log, Now: time.Now, token: store.NewULID()}
}

func (w *Worker) Run(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	eventsCh := w.Bus.Subscribe(runCtx, false)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	wake := make(chan struct{}, 1)
	deliveryDone := make(chan struct{})
	go func() {
		defer close(deliveryDone)
		for {
			select {
			case <-runCtx.Done():
				return
			case <-wake:
				if !w.live.Load() {
					w.deliver(runCtx)
				}
			}
		}
	}()
	signal := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	defer func() {
		cancel()
		<-deliveryDone
	}()
	signal()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-eventsCh:
			if !ok {
				return
			}
			if event.Kind == events.KindSessionState {
				var state struct {
					State string `json:"state"`
				}
				if json.Unmarshal(event.Payload, &state) == nil {
					live := state.State == "LIVE" || state.State == "QUIET" || state.State == "PARKING"
					w.setLive(live)
					if !live {
						signal()
					}
				}
				continue
			}
			if !w.live.Load() && noteworthy(event.Kind) {
				signal()
			}
		case <-ticker.C:
			if !w.live.Load() {
				signal()
			}
		}
	}
}

func noteworthy(kind string) bool {
	switch kind {
	case events.KindJobDone, events.KindJobFailed, events.KindJobNeedsInput,
		events.KindJobNeedsApproval, events.KindJobApprovalParked:
		return true
	default:
		return false
	}
}

func (w *Worker) setLive(live bool) {
	w.live.Store(live)
	if !live {
		return
	}
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	if w.active != nil {
		w.active.cancel()
	}
}

func (w *Worker) send(ctx context.Context, payload []byte, delivery store.PushDelivery) (int, error) {
	sendCtx, cancel := context.WithCancel(ctx)
	active := &activeSend{cancel: cancel}
	w.sendMu.Lock()
	if w.live.Load() {
		w.sendMu.Unlock()
		cancel()
		return 0, errVoiceBecameLive
	}
	w.active = active
	w.sendMu.Unlock()

	status, err := w.Sender.Send(sendCtx, payload, delivery)
	cancelled := sendCtx.Err() != nil
	cancel()
	w.sendMu.Lock()
	if w.active == active {
		w.active = nil
	}
	w.sendMu.Unlock()
	if err != nil && cancelled && ctx.Err() == nil && w.live.Load() {
		return 0, errVoiceBecameLive
	}
	return status, err
}

type notificationPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Tag   string `json:"tag"`
	URL   string `json:"url"`
}

func (w *Worker) deliver(ctx context.Context) {
	if w.live.Load() {
		return
	}
	deliveries, err := w.Store.ClaimPushDeliveries(ctx, w.token, batchSize, claimLease)
	if err != nil {
		w.Log.Warn("push delivery claim failed", "err", err)
		return
	}
	for _, delivery := range deliveries {
		if w.live.Load() {
			if err := w.Store.ReleasePushClaim(ctx, w.token); err != nil {
				w.Log.Warn("push delivery claim release failed", "err", err)
			}
			return
		}
		payload, err := json.Marshal(notificationPayload{
			Title: delivery.Title,
			Body:  delivery.Body,
			Tag:   fmt.Sprintf("thornhill-attention-%d", delivery.AttentionID),
			URL:   fmt.Sprintf("/?attention=%d", delivery.AttentionID),
		})
		if err != nil {
			w.fail(ctx, delivery, 0, err)
			continue
		}
		status, sendErr := w.send(ctx, payload, delivery)
		if sendErr == nil {
			if err := w.Store.MarkPushDelivered(ctx, w.token, delivery.ID); err != nil {
				w.Log.Warn("push delivery acknowledgement failed", "delivery_id", delivery.ID, "err", err)
			}
			continue
		}
		if errors.Is(sendErr, errVoiceBecameLive) {
			if err := w.Store.ReleasePushClaim(ctx, w.token); err != nil {
				w.Log.Warn("push delivery claim release failed", "err", err)
			}
			return
		}
		if ctx.Err() != nil {
			w.releaseClaimsAfterCancellation()
			return
		}
		if status == http.StatusNotFound || status == http.StatusGone {
			if err := w.Store.DisablePushSubscription(ctx, delivery.SubscriptionID); err != nil {
				w.Log.Warn("push subscription disable failed", "subscription_id", delivery.SubscriptionID, "err", err)
			}
			continue
		}
		if retryable(status) && delivery.Attempts < maxDeliveryAttempts {
			w.fail(ctx, delivery, status, sendErr)
			continue
		}
		w.abandon(ctx, delivery, status, sendErr)
	}
}

func retryable(status int) bool {
	return status == 0 || status == http.StatusTooManyRequests || status >= 500 && status <= 599
}

func (w *Worker) releaseClaimsAfterCancellation() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Store.ReleasePushClaim(ctx, w.token); err != nil {
		w.Log.Warn("push delivery claim release failed during shutdown", "err", err)
	}
}

func safeDeliveryError(status int, err error) string {
	if status > 0 {
		return fmt.Sprintf("push provider returned HTTP %d", status)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "push provider request timed out"
	case errors.Is(err, context.Canceled):
		return "push provider request was cancelled"
	default:
		// Transport errors commonly include the full endpoint URL. Its path is a
		// bearer capability, so never persist or log the raw error text.
		return "push provider transport failed"
	}
}

func (w *Worker) fail(ctx context.Context, delivery store.PushDelivery, status int, sendErr error) {
	delay := retryDelay(delivery.Attempts)
	message := safeDeliveryError(status, sendErr)
	if err := w.Store.MarkPushFailed(ctx, w.token, delivery.ID, w.Now().UTC().Add(delay), message); err != nil {
		w.Log.Warn("push delivery failure recording failed", "delivery_id", delivery.ID, "err", err)
		return
	}
	w.Log.Warn("push delivery failed", "delivery_id", delivery.ID, "status", status, "retry_in", delay, "error", message)
}

func (w *Worker) abandon(ctx context.Context, delivery store.PushDelivery, status int, sendErr error) {
	message := safeDeliveryError(status, sendErr)
	if err := w.Store.MarkPushAbandoned(ctx, w.token, delivery.ID, message); err != nil {
		w.Log.Warn("push terminal failure recording failed", "delivery_id", delivery.ID, "err", err)
		return
	}
	w.Log.Warn("push delivery abandoned", "delivery_id", delivery.ID, "status", status,
		"attempts", delivery.Attempts, "error", message)
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<(attempt-1)) * 30 * time.Second
}
