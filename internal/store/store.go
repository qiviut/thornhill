// Package store owns Postgres: schema, job records, the event log,
// rolling summaries and the usage ledger. Job identity is a ULID that
// never changes; display_name is mutable and is the voice handle.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"thornhill/internal/events"
)

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
    id                TEXT PRIMARY KEY,
    display_name      TEXT NOT NULL,
    task              TEXT NOT NULL,
    status            TEXT NOT NULL, -- queued|running|needs_input|needs_approval|parked_approval|done|failed|cancelled
    question          TEXT NOT NULL DEFAULT '',
    pending_input     TEXT NOT NULL DEFAULT '',
    result_digest     TEXT NOT NULL DEFAULT '',
    error             TEXT NOT NULL DEFAULT '',
    hermes_session_id TEXT NOT NULL DEFAULT '',
    hermes_run_id     TEXT NOT NULL DEFAULT '',
    approvals         JSONB NOT NULL DEFAULT '[]'::jsonb,
    progress          JSONB,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at       TIMESTAMPTZ
);
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS hermes_run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS pending_input TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS approvals JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS progress JSONB;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS state_version BIGINT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS jobs_display_name_lower_created_idx
    ON jobs (lower(display_name), created_at DESC);
CREATE INDEX IF NOT EXISTS jobs_active_created_idx
    ON jobs (created_at DESC)
    WHERE status IN ('queued','running','needs_input','needs_approval','parked_approval');
CREATE TABLE IF NOT EXISTS approval_denials (
    pattern_key   TEXT PRIMARY KEY,
    source_job_id TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS approval_allows (
    pattern_key   TEXT PRIMARY KEY,
    source_job_id TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS deployment_control (
    singleton       BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    dispatch_paused BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO deployment_control (singleton, dispatch_paused)
VALUES (TRUE, FALSE) ON CONFLICT (singleton) DO NOTHING;
CREATE OR REPLACE FUNCTION thornhill_guard_job_dispatch() RETURNS trigger
LANGUAGE plpgsql AS $guard$
BEGIN
	IF TG_OP = 'UPDATE' AND NEW.status IS NOT DISTINCT FROM OLD.status THEN
		RETURN NEW;
	END IF;
	IF NEW.status NOT IN ('queued', 'running') THEN
		RETURN NEW;
	END IF;
    PERFORM pg_advisory_xact_lock(72623859790382856);
    IF (SELECT dispatch_paused FROM deployment_control WHERE singleton=TRUE) THEN
        RAISE EXCEPTION USING ERRCODE='55000', MESSAGE='job dispatch is temporarily paused for deployment';
    END IF;
    RETURN NEW;
END
$guard$;
DROP TRIGGER IF EXISTS thornhill_guard_job_insert_trigger ON jobs;
DROP TRIGGER IF EXISTS thornhill_guard_job_dispatch_trigger ON jobs;
DROP FUNCTION IF EXISTS thornhill_guard_job_insert();
CREATE TRIGGER thornhill_guard_job_dispatch_trigger
BEFORE INSERT OR UPDATE OF status ON jobs
FOR EACH ROW EXECUTE FUNCTION thornhill_guard_job_dispatch();
CREATE TABLE IF NOT EXISTS event_log (
    seq     BIGSERIAL PRIMARY KEY,
    ts      TIMESTAMPTZ NOT NULL,
    kind    TEXT NOT NULL,
    job_id  TEXT NOT NULL DEFAULT '',
    payload JSONB
);
CREATE TABLE IF NOT EXISTS summaries (
    scope      TEXT PRIMARY KEY, -- 'rolling' | 'debrief'
    content    TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS attention_events (
    id             BIGSERIAL PRIMARY KEY,
    job_id         TEXT NOT NULL REFERENCES jobs(id),
    job_version    BIGINT NOT NULL,
    kind           TEXT NOT NULL,
    speech_text    TEXT NOT NULL,
    push_title     TEXT NOT NULL,
    push_body      TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    claim_token    TEXT NOT NULL DEFAULT '',
    claim_until    TIMESTAMPTZ,
    spoken_at      TIMESTAMPTZ,
    UNIQUE (job_id, job_version, kind)
);
CREATE INDEX IF NOT EXISTS attention_events_pending_idx
    ON attention_events (id) WHERE spoken_at IS NULL;
CREATE TABLE IF NOT EXISTS push_subscriptions (
    id          BIGSERIAL PRIMARY KEY,
    endpoint    TEXT NOT NULL UNIQUE,
    p256dh      TEXT NOT NULL,
    auth        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS push_deliveries (
    id              BIGSERIAL PRIMARY KEY,
    attention_id    BIGINT NOT NULL REFERENCES attention_events(id) ON DELETE CASCADE,
    subscription_id BIGINT NOT NULL REFERENCES push_subscriptions(id) ON DELETE CASCADE,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claim_token     TEXT NOT NULL DEFAULT '',
    claim_until     TIMESTAMPTZ,
    sent_at         TIMESTAMPTZ,
    failed_at       TIMESTAMPTZ,
    last_error      TEXT NOT NULL DEFAULT '',
    UNIQUE (attention_id, subscription_id)
);
ALTER TABLE push_deliveries ADD COLUMN IF NOT EXISTS failed_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS push_deliveries_ready_idx
    ON push_deliveries (next_attempt_at, id) WHERE sent_at IS NULL AND failed_at IS NULL;
CREATE TABLE IF NOT EXISTS usage_ledger (
    id            BIGSERIAL PRIMARY KEY,
    ts            TIMESTAMPTZ NOT NULL DEFAULT now(),
    source        TEXT NOT NULL, -- realtime|tts|summary|hermes
    input_tokens  BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    est_usd       DOUBLE PRECISION NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS usage_ledger_ts_idx ON usage_ledger (ts);
`

type Job struct {
	ID              string     `json:"id"`
	DisplayName     string     `json:"display_name"`
	Task            string     `json:"task"`
	Status          string     `json:"status"`
	Question        string     `json:"question,omitempty"`
	PendingInput    string     `json:"-"`
	ResultDigest    string     `json:"result_digest,omitempty"`
	Error           string     `json:"error,omitempty"`
	HermesSessionID string     `json:"hermes_session_id,omitempty"`
	HermesRunID     string     `json:"hermes_run_id,omitempty"`
	Approvals       []Approval `json:"approvals,omitempty"`
	Progress        *Progress  `json:"progress,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	StateVersion    int64      `json:"state_version"`
}

// Attention is a durable operator-attention obligation created in the same
// transaction as a noteworthy job transition. A voice call leases rows and
// acknowledges them only after completed output audio.
type Attention struct {
	ID         int64
	JobID      string
	JobVersion int64
	Kind       string
	SpeechText string
	PushTitle  string
	PushBody   string
	CreatedAt  time.Time
}

type PushSubscription struct {
	Endpoint string
	P256DH   string
	Auth     string
}

type PushDelivery struct {
	ID             int64
	SubscriptionID int64
	Endpoint       string
	P256DH         string
	Auth           string
	Title          string
	Body           string
	AttentionID    int64
	Attempts       int
}

// Approval is a redacted, user-visible authority request from Hermes. The
// Runs API resolves approvals FIFO, so callers may decide only the first entry.
type Approval struct {
	ID             string     `json:"id"`
	DecisionNonce  string     `json:"decision_nonce"`
	State          string     `json:"state"` // pending|sending|parked|indeterminate
	Command        string     `json:"command,omitempty"`
	Description    string     `json:"description,omitempty"`
	PatternKeys    []string   `json:"pattern_keys,omitempty"`
	AllowPermanent bool       `json:"allow_permanent"`
	RequestedAt    time.Time  `json:"requested_at"`
	ParkedAt       *time.Time `json:"parked_at,omitempty"`
	ParkReason     string     `json:"park_reason,omitempty"`
}

// Progress is the most recent structured Hermes tool lifecycle event.
type Progress struct {
	Tool      string    `json:"tool,omitempty"`
	Label     string    `json:"label,omitempty"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
}

const (
	StatusQueued         = "queued"
	StatusRunning        = "running"
	StatusNeedsInput     = "needs_input"
	StatusNeedsApproval  = "needs_approval"
	StatusParkedApproval = "parked_approval"
	StatusDone           = "done"
	StatusFailed         = "failed"
	StatusCancelled      = "cancelled"
)

const (
	ApprovalStatePending       = "pending"
	ApprovalStateSending       = "sending"
	ApprovalStateParked        = "parked"
	ApprovalStateIndeterminate = "indeterminate"
)

var ErrNotFound = errors.New("not found")
var ErrAmbiguous = errors.New("ambiguous job reference")
var ErrApprovalStale = errors.New("stale, replayed, or mismatched approval decision")
var ErrDispatchPaused = errors.New("job dispatch is temporarily paused for deployment")

type Store struct {
	Pool *pgxpool.Pool
	log  *slog.Logger
}

func Open(ctx context.Context, url string, log *slog.Logger) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("pgx pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	log.Info("store ready")
	return &Store{Pool: pool, log: log}, nil
}

func NewULID() string { return ulid.MustNew(ulid.Now(), rand.Reader).String() }

// --- events.Persister ---

func (s *Store) AppendEvent(ctx context.Context, e events.Event) error {
	_, err := s.Pool.Exec(ctx,
		`INSERT INTO event_log (ts, kind, job_id, payload) VALUES ($1,$2,$3,$4)`,
		e.TS, e.Kind, e.JobID, nullableJSON(e.Payload))
	return err
}

func nullableJSON(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return r
}

// --- jobs ---

func (s *Store) CreateJob(ctx context.Context, name, task string) (Job, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback(ctx)
	j, err := s.CreateJobTx(ctx, tx, name, task)
	if err != nil {
		return Job{}, err
	}
	return j, tx.Commit(ctx)
}

// CreateJobTx inserts a queued job in the caller's transaction. Dispatch uses
// this with River's InsertTx so the durable record and delivery commit together.
func (s *Store) CreateJobTx(ctx context.Context, tx pgx.Tx, name, task string) (Job, error) {
	now := time.Now().UTC()
	j := Job{ID: NewULID(), DisplayName: name, Task: task, Status: StatusQueued,
		CreatedAt: now, UpdatedAt: now}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(72623859790382856)`); err != nil {
		return Job{}, err
	}
	var paused bool
	if err := tx.QueryRow(ctx,
		`SELECT dispatch_paused FROM deployment_control WHERE singleton=TRUE FOR SHARE`).Scan(&paused); err != nil {
		return Job{}, err
	}
	if paused {
		return Job{}, ErrDispatchPaused
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO jobs (id, display_name, task, status, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		j.ID, j.DisplayName, j.Task, j.Status, j.CreatedAt, j.UpdatedAt); err != nil {
		return Job{}, normalizeDispatchError(err)
	}
	return j, nil
}

func (s *Store) UpdateJob(ctx context.Context, id string, mut func(*Job)) (Job, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback(ctx)
	j, err := s.UpdateJobTx(ctx, tx, id, mut)
	if err != nil {
		return Job{}, err
	}
	return j, tx.Commit(ctx)
}

// UpdateJobTx locks and updates a job inside the caller's transaction.
func (s *Store) UpdateJobTx(ctx context.Context, tx pgx.Tx, id string, mut func(*Job)) (Job, error) {
	j, err := scanJob(tx.QueryRow(ctx, selectJob+` WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return Job{}, err
	}
	previousStatus := j.Status
	mut(&j)
	j.StateVersion++
	j.UpdatedAt = time.Now().UTC()
	terminal := j.Status == StatusDone || j.Status == StatusFailed || j.Status == StatusCancelled
	if terminal && j.FinishedAt == nil {
		now := time.Now().UTC()
		j.FinishedAt = &now
	}
	approvals, err := json.Marshal(j.Approvals)
	if err != nil {
		return Job{}, fmt.Errorf("marshal approvals: %w", err)
	}
	var progress any
	if j.Progress != nil {
		progress, err = json.Marshal(j.Progress)
		if err != nil {
			return Job{}, fmt.Errorf("marshal progress: %w", err)
		}
	}
	_, err = tx.Exec(ctx, `UPDATE jobs SET display_name=$2, status=$3, question=$4,
		result_digest=$5, error=$6, hermes_session_id=$7, updated_at=$8, finished_at=$9,
		hermes_run_id=$10, approvals=$11, progress=$12, pending_input=$13, state_version=$14 WHERE id=$1`,
		j.ID, j.DisplayName, j.Status, j.Question, j.ResultDigest, j.Error,
		j.HermesSessionID, j.UpdatedAt, j.FinishedAt, j.HermesRunID, approvals, progress, j.PendingInput, j.StateVersion)
	if err != nil {
		return Job{}, normalizeDispatchError(err)
	}
	if previousStatus != j.Status {
		if a, ok := attentionForTransition(j); ok {
			if _, err := tx.Exec(ctx, `INSERT INTO attention_events
				(job_id, job_version, kind, speech_text, push_title, push_body, created_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7)
				ON CONFLICT (job_id, job_version, kind) DO NOTHING`,
				j.ID, j.StateVersion, a.Kind, a.SpeechText, a.PushTitle, a.PushBody, j.UpdatedAt); err != nil {
				return Job{}, fmt.Errorf("record attention transition: %w", err)
			}
		}
	}
	return j, nil
}

func attentionForTransition(j Job) (Attention, bool) {
	name := compactAttentionText(j.DisplayName, 80)
	a := Attention{JobID: j.ID, JobVersion: j.StateVersion, Kind: j.Status}
	switch j.Status {
	case StatusDone:
		a.SpeechText = fmt.Sprintf("Job %q finished. Quoted result digest: %s", name, compactAttentionText(j.ResultDigest, 500))
		a.PushTitle, a.PushBody = "Thornhill job finished", "A background job finished. Open Thornhill for details."
	case StatusFailed:
		a.SpeechText = fmt.Sprintf("Job %q failed. Quoted error: %s", name, compactAttentionText(j.Error, 500))
		a.PushTitle, a.PushBody = "Thornhill job failed", "A background job failed. Open Thornhill for details."
	case StatusNeedsInput:
		a.SpeechText = fmt.Sprintf("Job %q needs input. Quoted question: %s", name, compactAttentionText(j.Question, 500))
		a.PushTitle, a.PushBody = "Thornhill needs input", "A background job is waiting for input."
	case StatusNeedsApproval:
		a.SpeechText = fmt.Sprintf("Job %q needs approval. Retrieve the redacted pending request with job_status before asking for a decision.", name)
		a.PushTitle, a.PushBody = "Thornhill needs approval", "A background job is waiting for approval."
	case StatusParkedApproval:
		a.SpeechText = fmt.Sprintf("Job %q parked an unresolved approval without making a decision. Offer to resume the job with fresh authority.", name)
		a.PushTitle, a.PushBody = "Thornhill approval parked", "A background job released its run with an unresolved approval."
	default:
		return Attention{}, false
	}
	return a, true
}

func compactAttentionText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "(no details provided)"
	}
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}

func normalizeDispatchError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55000" && pgErr.Message == ErrDispatchPaused.Error() {
		return ErrDispatchPaused
	}
	return err
}

// ClaimApproval atomically validates and consumes the one-use decision nonce.
// UpdateJob holds SELECT ... FOR UPDATE for the complete check-and-set.
func (s *Store) ClaimApproval(ctx context.Context, jobID, approvalID, nonce string) (Job, error) {
	claimed := false
	j, err := s.UpdateJob(ctx, jobID, func(x *Job) {
		if x.Status == StatusNeedsApproval && len(x.Approvals) == 1 &&
			x.Approvals[0].ID == approvalID && x.Approvals[0].DecisionNonce == nonce &&
			x.Approvals[0].State == ApprovalStatePending {
			x.Approvals[0].State = ApprovalStateSending
			claimed = true
		}
	})
	if err != nil {
		return j, err
	}
	if !claimed {
		return j, ErrApprovalStale
	}
	return j, nil
}

// ParkApproval atomically competes with ClaimApproval for the same one-use
// nonce. Parking preserves the unresolved request as durable evidence but does
// not consume an authority decision.
func (s *Store) ParkApproval(ctx context.Context, jobID, approvalID, nonce, reason string, at time.Time) (Job, error) {
	parked := false
	at = at.UTC()
	j, err := s.UpdateJob(ctx, jobID, func(x *Job) {
		if x.Status == StatusNeedsApproval && len(x.Approvals) == 1 &&
			x.Approvals[0].ID == approvalID && x.Approvals[0].DecisionNonce == nonce &&
			x.Approvals[0].State == ApprovalStatePending {
			x.Status = StatusParkedApproval
			x.Approvals[0].State = ApprovalStateParked
			x.Approvals[0].ParkedAt = &at
			x.Approvals[0].ParkReason = reason
			x.Progress = &Progress{
				Tool:      "approval",
				State:     ApprovalStateParked,
				Label:     "approval parked unresolved; resume requires a fresh authority request",
				UpdatedAt: at,
			}
			parked = true
		}
	})
	if err != nil {
		return j, err
	}
	if !parked {
		return j, ErrApprovalStale
	}
	return j, nil
}

const selectJob = `SELECT id, display_name, task, status, question, result_digest,
	error, hermes_session_id, hermes_run_id, approvals, progress, pending_input,
	created_at, updated_at, finished_at, state_version FROM jobs`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	var approvals, progress []byte
	err := row.Scan(&j.ID, &j.DisplayName, &j.Task, &j.Status, &j.Question,
		&j.ResultDigest, &j.Error, &j.HermesSessionID, &j.HermesRunID,
		&approvals, &progress, &j.PendingInput, &j.CreatedAt, &j.UpdatedAt, &j.FinishedAt, &j.StateVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	if len(approvals) > 0 {
		if err := json.Unmarshal(approvals, &j.Approvals); err != nil {
			return j, fmt.Errorf("decode approvals: %w", err)
		}
	}
	if len(progress) > 0 && string(progress) != "null" {
		var p Progress
		if err := json.Unmarshal(progress, &p); err != nil {
			return j, fmt.Errorf("decode progress: %w", err)
		}
		j.Progress = &p
	}
	return j, nil
}

// ResolveJob turns a spoken reference into a job: exact ULID first, then
// case-insensitive exact name, then substring on active jobs. Ambiguity is
// surfaced, not guessed away — the desk model asks the human instead.
func (s *Store) ResolveJob(ctx context.Context, ref string) (Job, error) {
	ref = strings.TrimSpace(ref)
	if j, err := scanJob(s.Pool.QueryRow(ctx, selectJob+` WHERE id=$1`, ref)); err == nil {
		return j, nil
	}
	// Only two rows are needed to distinguish a unique match from ambiguity.
	rows, err := s.Pool.Query(ctx, selectJob+` WHERE lower(display_name)=lower($1) ORDER BY created_at DESC LIMIT 2`, ref)
	if err != nil {
		return Job{}, err
	}
	js, err := collect(rows)
	if err != nil {
		return Job{}, err
	}
	if len(js) == 1 {
		return js[0], nil
	}
	if len(js) > 1 {
		return Job{}, ErrAmbiguous
	}
	rows, err = s.Pool.Query(ctx, selectJob+
		` WHERE display_name ILIKE '%'||$1||'%' AND status IN ('queued','running','needs_input','needs_approval','parked_approval')
		  ORDER BY created_at DESC LIMIT 2`, ref)
	if err != nil {
		return Job{}, err
	}
	js, err = collect(rows)
	if err != nil {
		return Job{}, err
	}
	switch len(js) {
	case 0:
		return Job{}, ErrNotFound
	case 1:
		return js[0], nil
	default:
		return Job{}, ErrAmbiguous
	}
}

func collect(rows pgx.Rows) ([]Job, error) {
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ActiveJobs returns every non-terminal job, newest first. This is deliberately
// unbounded: hiding queued or authority-bearing work would make the operator's
// live board incomplete. The partial active-status index keeps the scan scoped.
func (s *Store) ActiveJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.Pool.Query(ctx, selectJob+
		` WHERE status IN ('queued','running','needs_input','needs_approval','parked_approval') ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

func (s *Store) RecentJobs(ctx context.Context, n int) ([]Job, error) {
	rows, err := s.Pool.Query(ctx, selectJob+` ORDER BY created_at DESC LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

// SavePermanentDenials persists exact approval pattern keys. Thornhill applies
// these before prompting; no prefix or glob matching is permitted.
// ApprovalPatternHash identifies the complete, normalized pattern-key set.
// Empty sets have no reusable scope and return an empty string.
func ApprovalPatternHash(patternKeys []string) string {
	keys := make([]string, 0, len(patternKeys))
	seen := map[string]struct{}{}
	for _, key := range patternKeys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	b, _ := json.Marshal(keys)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("v1:%x", sum[:])
}

func (s *Store) SavePermanentDenials(ctx context.Context, patternKeys []string, sourceJobID string) error {
	hash := ApprovalPatternHash(patternKeys)
	if hash == "" {
		return errors.New("approval has no reusable pattern keys")
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO approval_denials (pattern_key, source_job_id)
		VALUES ($1,$2) ON CONFLICT (pattern_key) DO UPDATE SET source_job_id=EXCLUDED.source_job_id`, hash, sourceJobID)
	return err
}

// MatchesPermanentDenial requires equality of the complete normalized set.
func (s *Store) MatchesPermanentDenial(ctx context.Context, patternKeys []string) (string, error) {
	hash := ApprovalPatternHash(patternKeys)
	if hash == "" {
		return "", nil
	}
	var found string
	err := s.Pool.QueryRow(ctx, `SELECT pattern_key FROM approval_denials WHERE pattern_key=$1`, hash).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return found, err
}

func (s *Store) SavePermanentAllows(ctx context.Context, patternKeys []string, sourceJobID string) error {
	hash := ApprovalPatternHash(patternKeys)
	if hash == "" {
		return errors.New("approval has no reusable pattern keys")
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO approval_allows (pattern_key, source_job_id)
		VALUES ($1,$2) ON CONFLICT (pattern_key) DO UPDATE SET source_job_id=EXCLUDED.source_job_id`, hash, sourceJobID)
	return err
}

func (s *Store) MatchesPermanentAllow(ctx context.Context, patternKeys []string) (string, error) {
	hash := ApprovalPatternHash(patternKeys)
	if hash == "" {
		return "", nil
	}
	var found string
	err := s.Pool.QueryRow(ctx, `SELECT pattern_key FROM approval_allows WHERE pattern_key=$1`, hash).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return found, err
}

// --- durable operator attention and Web Push outbox ---

func (s *Store) ClaimPendingAttention(ctx context.Context, token string, limit int, lease time.Duration) ([]Attention, error) {
	if token == "" {
		return nil, errors.New("attention claim token is required")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	until := time.Now().UTC().Add(lease)
	rows, err := s.Pool.Query(ctx, `WITH candidates AS (
		SELECT id FROM attention_events
		WHERE spoken_at IS NULL AND (claim_until IS NULL OR claim_until < now())
		ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $2
	)
	UPDATE attention_events AS a
	SET claim_token=$1, claim_until=$3
	FROM candidates c WHERE a.id=c.id
	RETURNING a.id, a.job_id, a.job_version, a.kind, a.speech_text,
		a.push_title, a.push_body, a.created_at`, token, limit, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attention
	for rows.Next() {
		var a Attention
		if err := rows.Scan(&a.ID, &a.JobID, &a.JobVersion, &a.Kind, &a.SpeechText,
			&a.PushTitle, &a.PushBody, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ReleaseAttentionClaim(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `UPDATE attention_events
		SET claim_token='', claim_until=NULL
		WHERE claim_token=$1 AND spoken_at IS NULL`, token)
	return err
}

func (s *Store) MarkAttentionSpoken(ctx context.Context, token string, ids []int64) (int64, error) {
	if token == "" || len(ids) == 0 {
		return 0, nil
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE attention_events
		SET spoken_at=now(), claim_token='', claim_until=NULL
		WHERE claim_token=$1 AND id=ANY($2) AND spoken_at IS NULL`, token, ids)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) UpsertPushSubscription(ctx context.Context, sub PushSubscription) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id int64
	var disabled bool
	err = tx.QueryRow(ctx, `SELECT id, disabled_at IS NOT NULL FROM push_subscriptions
		WHERE endpoint=$1 FOR UPDATE`, sub.Endpoint).Scan(&id, &disabled)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `INSERT INTO push_subscriptions (endpoint, p256dh, auth)
			VALUES ($1,$2,$3) ON CONFLICT (endpoint) DO UPDATE SET
			p256dh=EXCLUDED.p256dh, auth=EXCLUDED.auth, updated_at=now()`,
			sub.Endpoint, sub.P256DH, sub.Auth)
	case err != nil:
		return err
	case disabled:
		// Reactivation starts a new opt-in epoch. Pending deliveries tied to the
		// revoked capability must not replay when the browser enrolls again.
		if _, err = tx.Exec(ctx, `DELETE FROM push_deliveries
			WHERE subscription_id=$1 AND sent_at IS NULL`, id); err == nil {
			_, err = tx.Exec(ctx, `UPDATE push_subscriptions SET p256dh=$2, auth=$3,
				created_at=now(), updated_at=now(), disabled_at=NULL WHERE id=$1`,
				id, sub.P256DH, sub.Auth)
		}
	default:
		_, err = tx.Exec(ctx, `UPDATE push_subscriptions SET p256dh=$2, auth=$3,
			updated_at=now() WHERE id=$1`, id, sub.P256DH, sub.Auth)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) DeletePushSubscription(ctx context.Context, endpoint string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE endpoint=$1`, endpoint)
	return err
}

func (s *Store) ClaimPushDeliveries(ctx context.Context, token string, limit int, lease time.Duration) ([]PushDelivery, error) {
	if token == "" {
		return nil, errors.New("push claim token is required")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// A subscription receives only attention created after it opted in. This
	// avoids surprising historical notifications on first registration.
	if _, err := tx.Exec(ctx, `INSERT INTO push_deliveries (attention_id, subscription_id)
		SELECT a.id, s.id FROM attention_events a CROSS JOIN push_subscriptions s
		WHERE a.spoken_at IS NULL AND s.disabled_at IS NULL AND a.created_at >= s.created_at
		ON CONFLICT (attention_id, subscription_id) DO NOTHING`); err != nil {
		return nil, err
	}
	until := time.Now().UTC().Add(lease)
	rows, err := tx.Query(ctx, `WITH candidates AS (
		SELECT d.id FROM push_deliveries d
		JOIN attention_events a ON a.id=d.attention_id
		JOIN push_subscriptions s ON s.id=d.subscription_id
		WHERE d.sent_at IS NULL AND d.failed_at IS NULL
		  AND a.spoken_at IS NULL AND s.disabled_at IS NULL
		  AND d.next_attempt_at <= now()
		  AND (d.claim_until IS NULL OR d.claim_until < now())
		ORDER BY d.id FOR UPDATE OF d SKIP LOCKED LIMIT $2
	)
	UPDATE push_deliveries d SET claim_token=$1, claim_until=$3, attempts=d.attempts+1
	FROM candidates c, attention_events a, push_subscriptions s
	WHERE d.id=c.id AND a.id=d.attention_id AND s.id=d.subscription_id
	RETURNING d.id, d.subscription_id, s.endpoint, s.p256dh, s.auth,
		a.push_title, a.push_body, a.id, d.attempts`, token, limit, until)
	if err != nil {
		return nil, err
	}
	var out []PushDelivery
	for rows.Next() {
		var d PushDelivery
		if err := rows.Scan(&d.ID, &d.SubscriptionID, &d.Endpoint, &d.P256DH, &d.Auth,
			&d.Title, &d.Body, &d.AttentionID, &d.Attempts); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) ReleasePushClaim(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `UPDATE push_deliveries
		SET claim_token='', claim_until=NULL
		WHERE claim_token=$1 AND sent_at IS NULL`, token)
	return err
}

func (s *Store) MarkPushDelivered(ctx context.Context, token string, id int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE push_deliveries
		SET sent_at=now(), claim_token='', claim_until=NULL, last_error=''
		WHERE id=$1 AND claim_token=$2 AND sent_at IS NULL`, id, token)
	return err
}

func (s *Store) MarkPushFailed(ctx context.Context, token string, id int64, retryAt time.Time, message string) error {
	message = compactAttentionText(message, 300)
	_, err := s.Pool.Exec(ctx, `UPDATE push_deliveries
		SET next_attempt_at=$3, claim_token='', claim_until=NULL, last_error=$4
		WHERE id=$1 AND claim_token=$2 AND sent_at IS NULL`, id, token, retryAt.UTC(), message)
	return err
}

func (s *Store) MarkPushAbandoned(ctx context.Context, token string, id int64, message string) error {
	message = compactAttentionText(message, 300)
	_, err := s.Pool.Exec(ctx, `UPDATE push_deliveries
		SET failed_at=now(), claim_token='', claim_until=NULL, last_error=$3
		WHERE id=$1 AND claim_token=$2 AND sent_at IS NULL AND failed_at IS NULL`, id, token, message)
	return err
}

func (s *Store) DisablePushSubscription(ctx context.Context, subscriptionID int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE push_subscriptions SET disabled_at=now(), updated_at=now()
		WHERE id=$1`, subscriptionID)
	return err
}

// --- summaries ---

func (s *Store) SaveSummary(ctx context.Context, scope, content string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO summaries (scope, content, updated_at)
		VALUES ($1,$2,now())
		ON CONFLICT (scope) DO UPDATE SET content=EXCLUDED.content, updated_at=now()`,
		scope, content)
	return err
}

func (s *Store) GetSummary(ctx context.Context, scope string) (string, time.Time, error) {
	var content string
	var updated time.Time
	err := s.Pool.QueryRow(ctx,
		`SELECT content, updated_at FROM summaries WHERE scope=$1`, scope).Scan(&content, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, nil
	}
	return content, updated, err
}

// --- usage ---

func (s *Store) AddUsage(ctx context.Context, source string, in, out int64, estUSD float64) error {
	_, err := s.Pool.Exec(ctx,
		`INSERT INTO usage_ledger (source, input_tokens, output_tokens, est_usd) VALUES ($1,$2,$3,$4)`,
		source, in, out, estUSD)
	return err
}

func (s *Store) UsageTodayUSD(ctx context.Context) (float64, error) {
	var usd float64
	err := s.Pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(est_usd),0) FROM usage_ledger WHERE ts >= date_trunc('day', now())`).Scan(&usd)
	return usd, err
}
