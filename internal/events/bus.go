// Package events is Thornhill's spine: an append-only, in-process event
// bus with bounded replay. Everything observable (transcripts, job
// transitions, initiatives, errors) flows through here; the UI, the
// debrief summarizer and the desk all subscribe.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Kinds. Deliberately flat strings: grep-able, DB-friendly.
const (
	KindJobQueued              = "job.queued"
	KindJobRunning             = "job.running"
	KindJobNeedsInput          = "job.needs_input"
	KindJobNeedsApproval       = "job.needs_approval"
	KindJobApprovalParked      = "job.approval_parked"
	KindJobApprovalResolved    = "job.approval_resolved"
	KindJobApprovalAutoDenied  = "job.approval_auto_denied"
	KindJobApprovalAutoAllowed = "job.approval_auto_allowed"
	KindJobProgress            = "job.progress"
	KindJobDone                = "job.done"
	KindJobFailed              = "job.failed"
	KindJobCancelled           = "job.cancelled"
	KindJobRenamed             = "job.renamed"
	KindTranscriptIn           = "transcript.user"
	KindTranscriptOut          = "transcript.assistant"
	KindSessionState           = "session.state" // LIVE / QUIET / PARKED
	KindHermesHook             = "hermes.hook"
	KindErrorVoice             = "error.voice"
	KindUsage                  = "usage"
)

type Event struct {
	Seq     int64           `json:"seq"`
	TS      time.Time       `json:"ts"`
	Kind    string          `json:"kind"`
	JobID   string          `json:"job_id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Persister is implemented by the store; nil is fine (tests, early boot).
type Persister interface {
	AppendEvent(ctx context.Context, e Event) error
}

type persistItem struct {
	event Event
	done  chan struct{}
}

type Bus struct {
	mu      sync.RWMutex
	subs    map[int64]chan Event
	nextSub int64
	seq     atomic.Int64
	ring    []Event // bounded replay for late joiners (UI reconnects)
	ringCap int
	persist Persister
	log     *slog.Logger

	persistMu     sync.RWMutex
	publishMu     sync.Mutex
	persistQ      chan persistItem
	persistDone   chan struct{}
	persistClosed bool
}

func NewBus(persist Persister, log *slog.Logger) *Bus {
	b := &Bus{
		subs:    map[int64]chan Event{},
		ringCap: 256,
		persist: persist,
		log:     log,
	}
	if persist != nil {
		b.persistQ = make(chan persistItem, 256)
		b.persistDone = make(chan struct{})
		go b.persistLoop()
	}
	return b
}

func (b *Bus) persistOne(e Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.persist.AppendEvent(ctx, e); err != nil {
		b.log.Warn("event persist failed", "kind", e.Kind, "seq", e.Seq, "err", err)
	}
}

func (b *Bus) persistLoop() {
	defer close(b.persistDone)
	for item := range b.persistQ {
		b.persistOne(item.event)
		if item.done != nil {
			close(item.done)
		}
	}
}

func (b *Bus) enqueuePersistence(e Event, synchronous bool) <-chan struct{} {
	if b.persist == nil {
		return nil
	}
	b.persistMu.RLock()
	defer b.persistMu.RUnlock()
	if b.persistClosed {
		return nil
	}
	item := persistItem{event: e}
	if synchronous {
		item.done = make(chan struct{})
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case b.persistQ <- item:
			return item.done
		case <-timer.C:
			b.log.Warn("synchronous event persistence queue admission timed out", "kind", e.Kind, "seq", e.Seq)
			return nil
		}
	}
	select {
	case b.persistQ <- item:
	default:
		b.log.Warn("event persistence queue full; dropping best-effort event", "kind", e.Kind, "seq", e.Seq)
	}
	return nil
}

// Close drains queued persistence work. New publications remain observable in
// process but are no longer accepted for persistence after shutdown begins.
func (b *Bus) Close(ctx context.Context) error {
	if b.persist == nil {
		return nil
	}
	b.persistMu.Lock()
	if !b.persistClosed {
		b.persistClosed = true
		close(b.persistQ)
	}
	b.persistMu.Unlock()
	select {
	case <-b.persistDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Publish stamps, queues best-effort persistence and fans out without
// blocking. A slow persister or subscriber cannot stall the desk.
func (b *Bus) Publish(kind, jobID string, payload any) Event {
	return b.publish(kind, jobID, payload, false)
}

// PublishSync is reserved for transitions whose caller must not release its
// state fence until persistence and in-process publication have completed.
func (b *Bus) PublishSync(kind, jobID string, payload any) Event {
	return b.publish(kind, jobID, payload, true)
}

func (b *Bus) publish(kind, jobID string, payload any, syncPersistence bool) Event {
	var raw json.RawMessage
	if payload != nil {
		bts, err := json.Marshal(payload)
		if err != nil {
			b.log.Error("event payload marshal failed", "kind", kind, "err", err)
		} else {
			raw = bts
		}
	}
	b.publishMu.Lock()
	e := Event{Seq: b.seq.Add(1), TS: time.Now().UTC(), Kind: kind, JobID: jobID, Payload: raw}
	persisted := b.enqueuePersistence(e, syncPersistence)

	b.mu.Lock()
	b.ring = append(b.ring, e)
	if len(b.ring) > b.ringCap {
		b.ring = b.ring[len(b.ring)-b.ringCap:]
	}
	for id, ch := range b.subs {
		select {
		case ch <- e:
		default:
			b.log.Debug("subscriber lagging, dropping event", "sub", id, "kind", kind)
		}
	}
	b.mu.Unlock()
	b.publishMu.Unlock()
	b.log.Debug("event", "kind", kind, "job", jobID, "seq", e.Seq)

	if persisted != nil {
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-persisted:
		case <-timer.C:
			b.log.Warn("synchronous event persistence timed out", "kind", e.Kind, "seq", e.Seq)
		}
	}
	return e
}

// Subscribe returns a channel of live events plus a replay of the recent
// ring (so a reconnecting UI repaints without a DB round trip). Cancel the
// context to unsubscribe.
func (b *Bus) Subscribe(ctx context.Context, replay bool) <-chan Event {
	ch := make(chan Event, 64)
	b.mu.Lock()
	id := b.nextSub
	b.nextSub++
	b.subs[id] = ch
	var back []Event
	if replay {
		back = append(back, b.ring...)
	}
	b.mu.Unlock()

	out := make(chan Event, 64)
	go func() {
		defer close(out)
		for _, e := range back {
			select {
			case out <- e:
			case <-ctx.Done():
			}
		}
		for {
			select {
			case <-ctx.Done():
				b.mu.Lock()
				delete(b.subs, id)
				b.mu.Unlock()
				return
			case e := <-ch:
				select {
				case out <- e:
				case <-ctx.Done():
				}
			}
		}
	}()
	return out
}
