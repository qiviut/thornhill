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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"thornhill/internal/events"
)

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
    id                TEXT PRIMARY KEY,
    display_name      TEXT NOT NULL,
    task              TEXT NOT NULL,
    status            TEXT NOT NULL, -- queued|running|needs_input|needs_approval|done|failed|cancelled
    question          TEXT NOT NULL DEFAULT '',
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
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS approvals JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS progress JSONB;
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
CREATE OR REPLACE FUNCTION thornhill_guard_job_insert() RETURNS trigger
LANGUAGE plpgsql AS $guard$
BEGIN
    PERFORM pg_advisory_xact_lock(72623859790382856);
    IF (SELECT dispatch_paused FROM deployment_control WHERE singleton=TRUE) THEN
        RAISE EXCEPTION USING ERRCODE='55000', MESSAGE='job dispatch is temporarily paused for deployment';
    END IF;
    RETURN NEW;
END
$guard$;
DO $trigger$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname='thornhill_guard_job_insert_trigger') THEN
        CREATE TRIGGER thornhill_guard_job_insert_trigger
        BEFORE INSERT ON jobs FOR EACH ROW EXECUTE FUNCTION thornhill_guard_job_insert();
    END IF;
END
$trigger$;
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
CREATE TABLE IF NOT EXISTS usage_ledger (
    id            BIGSERIAL PRIMARY KEY,
    ts            TIMESTAMPTZ NOT NULL DEFAULT now(),
    source        TEXT NOT NULL, -- realtime|tts|summary|hermes
    input_tokens  BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    est_usd       DOUBLE PRECISION NOT NULL DEFAULT 0
);
`

type Job struct {
	ID              string     `json:"id"`
	DisplayName     string     `json:"display_name"`
	Task            string     `json:"task"`
	Status          string     `json:"status"`
	Question        string     `json:"question,omitempty"`
	ResultDigest    string     `json:"result_digest,omitempty"`
	Error           string     `json:"error,omitempty"`
	HermesSessionID string     `json:"hermes_session_id,omitempty"`
	HermesRunID     string     `json:"hermes_run_id,omitempty"`
	Approvals       []Approval `json:"approvals,omitempty"`
	Progress        *Progress  `json:"progress,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

// Approval is a redacted, user-visible authority request from Hermes. The
// Runs API resolves approvals FIFO, so callers may decide only the first entry.
type Approval struct {
	ID             string    `json:"id"`
	DecisionNonce  string    `json:"decision_nonce"`
	State          string    `json:"state"` // pending|sending|indeterminate
	Command        string    `json:"command,omitempty"`
	Description    string    `json:"description,omitempty"`
	PatternKeys    []string  `json:"pattern_keys,omitempty"`
	AllowPermanent bool      `json:"allow_permanent"`
	RequestedAt    time.Time `json:"requested_at"`
}

// Progress is the most recent structured Hermes tool lifecycle event.
type Progress struct {
	Tool      string    `json:"tool,omitempty"`
	Label     string    `json:"label,omitempty"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
}

const (
	StatusQueued        = "queued"
	StatusRunning       = "running"
	StatusNeedsInput    = "needs_input"
	StatusNeedsApproval = "needs_approval"
	StatusDone          = "done"
	StatusFailed        = "failed"
	StatusCancelled     = "cancelled"
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
	j := Job{ID: NewULID(), DisplayName: name, Task: task, Status: StatusQueued,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback(ctx)
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
		return Job{}, err
	}
	return j, tx.Commit(ctx)
}

func (s *Store) UpdateJob(ctx context.Context, id string, mut func(*Job)) (Job, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback(ctx)
	j, err := scanJob(tx.QueryRow(ctx, selectJob+` WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return Job{}, err
	}
	mut(&j)
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
		hermes_run_id=$10, approvals=$11, progress=$12 WHERE id=$1`,
		j.ID, j.DisplayName, j.Status, j.Question, j.ResultDigest, j.Error,
		j.HermesSessionID, j.UpdatedAt, j.FinishedAt, j.HermesRunID, approvals, progress)
	if err != nil {
		return Job{}, err
	}
	return j, tx.Commit(ctx)
}

// ClaimApproval atomically validates and consumes the one-use decision nonce.
// UpdateJob holds SELECT ... FOR UPDATE for the complete check-and-set.
func (s *Store) ClaimApproval(ctx context.Context, jobID, approvalID, nonce string) (Job, error) {
	claimed := false
	j, err := s.UpdateJob(ctx, jobID, func(x *Job) {
		if x.Status == StatusNeedsApproval && len(x.Approvals) == 1 &&
			x.Approvals[0].ID == approvalID && x.Approvals[0].DecisionNonce == nonce &&
			x.Approvals[0].State == "pending" {
			x.Approvals[0].State = "sending"
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

const selectJob = `SELECT id, display_name, task, status, question, result_digest,
	error, hermes_session_id, hermes_run_id, approvals, progress,
	created_at, updated_at, finished_at FROM jobs`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	var approvals, progress []byte
	err := row.Scan(&j.ID, &j.DisplayName, &j.Task, &j.Status, &j.Question,
		&j.ResultDigest, &j.Error, &j.HermesSessionID, &j.HermesRunID,
		&approvals, &progress, &j.CreatedAt, &j.UpdatedAt, &j.FinishedAt)
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
	rows, err := s.Pool.Query(ctx, selectJob+` WHERE lower(display_name)=lower($1) ORDER BY created_at DESC`, ref)
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
		` WHERE display_name ILIKE '%'||$1||'%' AND status IN ('queued','running','needs_input','needs_approval')
		  ORDER BY created_at DESC`, ref)
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

// ActiveJobs returns non-terminal jobs, newest first: this is the live job
// table the desk model keeps in context.
func (s *Store) ActiveJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.Pool.Query(ctx, selectJob+
		` WHERE status IN ('queued','running','needs_input','needs_approval') ORDER BY created_at DESC`)
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
