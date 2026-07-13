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

type Bus struct {
	mu      sync.RWMutex
	subs    map[int64]chan Event
	nextSub int64
	seq     atomic.Int64
	ring    []Event // bounded replay for late joiners (UI reconnects)
	ringCap int
	persist Persister
	log     *slog.Logger
}

func NewBus(persist Persister, log *slog.Logger) *Bus {
	return &Bus{
		subs:    map[int64]chan Event{},
		ringCap: 256,
		persist: persist,
		log:     log,
	}
}

// Publish stamps, persists (best effort) and fans out without blocking:
// a slow subscriber drops events rather than stalling the desk.
func (b *Bus) Publish(kind, jobID string, payload any) Event {
	var raw json.RawMessage
	if payload != nil {
		bts, err := json.Marshal(payload)
		if err != nil {
			b.log.Error("event payload marshal failed", "kind", kind, "err", err)
		} else {
			raw = bts
		}
	}
	e := Event{Seq: b.seq.Add(1), TS: time.Now().UTC(), Kind: kind, JobID: jobID, Payload: raw}

	if b.persist != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := b.persist.AppendEvent(ctx, e); err != nil {
			b.log.Warn("event persist failed", "kind", kind, "err", err)
		}
		cancel()
	}

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
	b.log.Debug("event", "kind", kind, "job", jobID, "seq", e.Seq)
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
