package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// Event is one record in the durable event log (chapter 9.1).
// Task 02 (Engine), 04 (tool runtime) and 06 reuse this type directly.
//
// Alignment (08 ruling A1/D-4): the authoritative definition lives in
// internal/types/event.go and this struct is field-for-field identical to it
// (names, types and JSON tags all match the run_events columns), so at assembly
// time the local definition becomes `type Event = types.Event` with zero
// further changes and no field can be silently dropped in a conversion.
type Event struct {
	// RunID is the owning run. Part of the primary key.
	RunID string `json:"run_id"`
	// Seq is the per-run monotonic sequence, starting at 1. An event
	// sequence must never go backwards (I4).
	Seq uint64 `json:"seq"`
	// EventID is the globally unique event id (uuid v4). UI reconnect
	// backfill and outbox dedup both rely on it.
	EventID string `json:"event_id"`
	// Type is the event type, e.g. "tool.completed", "run.started".
	Type string `json:"event_type"`
	// PayloadJSON is the event body. API keys and other secrets must never
	// appear here (I12).
	PayloadJSON json.RawMessage `json:"payload_json"`
	// CreatedAt is unix milliseconds.
	CreatedAt int64 `json:"created_at"`
}

// EventStore is the event log interface consumed by task 02/04 (contract,
// do not change the signature).
type EventStore interface {
	Append(ctx context.Context, runID string, eventType string, payload []byte) (seq uint64, err error)
	Since(ctx context.Context, runID string, afterSeq uint64) ([]Event, error)
}

// EventLog is the EventStore implementation backed by the run_events table.
type EventLog struct {
	db *sqlite.DB
}

// NewEventLog creates an EventLog.
func NewEventLog(db *sqlite.DB) *EventLog { return &EventLog{db: db} }

var (
	// ErrRunNotFound means the run does not exist (an event must never
	// become an orphan).
	ErrRunNotFound = errors.New("storage: run not found")
	// ErrSeqRegression means the explicit seq is not greater than the
	// current max seq (the I4 guard).
	ErrSeqRegression = errors.New("storage: event sequence regression")
	// ErrEventIDConflict means the event_id already exists.
	ErrEventIDConflict = errors.New("storage: duplicate event id")
)

// txExecer is the minimal SQL capability inside a transaction. Both *sql.Tx
// and *sqlite.WriteTx satisfy it, so composite transaction logic is written
// once for either transaction source.
type txExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Append appends one event and returns the assigned seq. The seq is taken as
// MAX(seq)+1 inside the write transaction (with runs.last_seq as a backstop)
// and committed together with the runs.last_seq update, guaranteeing I4: the
// sequence never goes backwards.
func (l *EventLog) Append(ctx context.Context, runID string, eventType string, payload []byte) (seq uint64, err error) {
	ev := Event{
		EventID:     newEventID(),
		RunID:       runID,
		Type:        eventType,
		PayloadJSON: payload,
		CreatedAt:   nowMS(),
	}
	err = l.db.WithTx(ctx, func(tx *sql.Tx) error {
		var e error
		seq, e = appendEventTx(ctx, tx, ev)
		return e
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// AppendTx appends an event inside the caller's transaction (the chapter 9.2
// composite transaction: tool_results + run_events + runs + outbox commit
// together). The seq is assigned inside the same transaction and the assigned
// event id is returned so the caller can enqueue the outbox row for the very
// same event.
func (l *EventLog) AppendTx(ctx context.Context, tx Tx, runID string, eventType string, payload []byte) (seq uint64, eventID string, err error) {
	ex, ok := tx.(txExecer)
	if !ok {
		return 0, "", fmt.Errorf("storage: unsupported Tx type %T", tx)
	}
	ev := Event{
		EventID:     newEventID(),
		RunID:       runID,
		Type:        eventType,
		PayloadJSON: payload,
		CreatedAt:   nowMS(),
	}
	seq, err = appendEventTx(ctx, ex, ev)
	if err != nil {
		return 0, "", err
	}
	return seq, ev.EventID, nil
}

// AppendWithSeq appends an event with an explicit seq (replay / legacy import
// only). The seq must be greater than the run's current max seq, otherwise
// ErrSeqRegression is returned (I4).
func (l *EventLog) AppendWithSeq(ctx context.Context, tx Tx, ev Event) error {
	ex, ok := tx.(txExecer)
	if !ok {
		return fmt.Errorf("storage: unsupported Tx type %T", tx)
	}
	maxSeq, err := maxEventSeq(ctx, ex, ev.RunID)
	if err != nil {
		return err
	}
	if ev.Seq <= maxSeq {
		return fmt.Errorf("%w: run=%s seq=%d max=%d", ErrSeqRegression, ev.RunID, ev.Seq, maxSeq)
	}
	if _, err := ex.ExecContext(ctx,
		"INSERT INTO run_events (run_id, seq, event_id, event_type, payload_json, created_at) VALUES (?,?,?,?,?,?)",
		ev.RunID, ev.Seq, ev.EventID, ev.Type, ev.PayloadJSON, ev.CreatedAt); err != nil {
		if IsUniqueViolation(err) {
			return fmt.Errorf("%w: %s", ErrEventIDConflict, ev.EventID)
		}
		return fmt.Errorf("storage: insert event: %w", err)
	}
	return bumpLastSeq(ctx, ex, ev.RunID, ev.Seq)
}

// Since returns the events of runID with seq > afterSeq, ordered by seq.
// This is the contract method and is uncapped; use SinceN for batched pulls.
func (l *EventLog) Since(ctx context.Context, runID string, afterSeq uint64) ([]Event, error) {
	rows, err := l.db.QueryContext(ctx,
		"SELECT seq, event_id, event_type, payload_json, created_at FROM run_events WHERE run_id=? AND seq>? ORDER BY seq ASC",
		runID, afterSeq)
	if err != nil {
		return nil, fmt.Errorf("storage: query events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		ev.RunID = runID
		if err := rows.Scan(&ev.Seq, &ev.EventID, &ev.Type, &ev.PayloadJSON, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("storage: scan event: %w", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate events: %w", err)
	}
	return out, nil
}

// DefaultSinceLimit is the explicit cap SinceN applies when limit <= 0. A UI
// reconnect must pull in batches — one unbounded pull would flood the frontend
// and the IPC channel — so "unlimited" is deliberately not offered here.
const DefaultSinceLimit = 10000

// SinceN is Since with a row cap (the UI pulls events in batches when
// reconnecting, so a single pull cannot flood the frontend). limit <= 0 (or
// above the cap) resolves to DefaultSinceLimit.
func (l *EventLog) SinceN(ctx context.Context, runID string, afterSeq uint64, limit int) ([]Event, error) {
	if limit <= 0 || limit > DefaultSinceLimit {
		limit = DefaultSinceLimit
	}
	query := "SELECT seq, event_id, event_type, payload_json, created_at FROM run_events WHERE run_id=? AND seq>? ORDER BY seq ASC"
	args := []any{runID, afterSeq}
	query += " LIMIT ?"
	args = append(args, limit)
	rows, err := l.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: query events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		ev.RunID = runID
		if err := rows.Scan(&ev.Seq, &ev.EventID, &ev.Type, &ev.PayloadJSON, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("storage: scan event: %w", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate events: %w", err)
	}
	return out, nil
}

// LastSeq returns the run's current committed max seq (0 when there are no
// events yet).
func (l *EventLog) LastSeq(ctx context.Context, runID string) (uint64, error) {
	var seq uint64
	row := l.db.QueryRowContext(ctx, "SELECT COALESCE(last_seq,0) FROM runs WHERE id=?", runID)
	if err := row.Scan(&seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: %s", ErrRunNotFound, runID)
		}
		return 0, fmt.Errorf("storage: read last_seq: %w", err)
	}
	return seq, nil
}

// ---------------------------------------------------------------- internal

func maxEventSeq(ctx context.Context, tx txExecer, runID string) (uint64, error) {
	rows, err := tx.QueryContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM run_events WHERE run_id=?", runID)
	if err != nil {
		return 0, fmt.Errorf("storage: read max seq: %w", err)
	}
	defer rows.Close()
	var maxSeq uint64
	if rows.Next() {
		if err := rows.Scan(&maxSeq); err != nil {
			return 0, fmt.Errorf("storage: scan max seq: %w", err)
		}
	}
	return maxSeq, rows.Err()
}

// appendEventTx assigns the seq inside the transaction, inserts the event and
// advances runs.last_seq. It must be called inside a serialized write
// transaction (guaranteed by the single write queue).
func appendEventTx(ctx context.Context, tx txExecer, ev Event) (uint64, error) {
	var maxEvSeq, lastSeq uint64
	rows, err := tx.QueryContext(ctx,
		"SELECT COALESCE((SELECT MAX(seq) FROM run_events WHERE run_id=?), 0), COALESCE((SELECT last_seq FROM runs WHERE id=?), 0)",
		ev.RunID, ev.RunID)
	if err != nil {
		return 0, fmt.Errorf("storage: read seq state: %w", err)
	}
	found := rows.Next()
	if found {
		if err := rows.Scan(&maxEvSeq, &lastSeq); err != nil {
			rows.Close()
			return 0, fmt.Errorf("storage: scan seq state: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("storage: iterate seq state: %w", err)
	}
	rows.Close()
	if !found {
		return 0, fmt.Errorf("%w: %s", ErrRunNotFound, ev.RunID)
	}

	seq := maxEvSeq
	if lastSeq > seq {
		seq = lastSeq
	}
	seq++ // I4: strictly greater than the committed maximum

	if _, err := tx.ExecContext(ctx,
		"INSERT INTO run_events (run_id, seq, event_id, event_type, payload_json, created_at) VALUES (?,?,?,?,?,?)",
		ev.RunID, seq, ev.EventID, ev.Type, ev.PayloadJSON, ev.CreatedAt); err != nil {
		if IsUniqueViolation(err) {
			return 0, fmt.Errorf("%w: %s", ErrEventIDConflict, ev.EventID)
		}
		return 0, fmt.Errorf("storage: insert event: %w", err)
	}
	if err := bumpLastSeq(ctx, tx, ev.RunID, seq); err != nil {
		return 0, err
	}
	return seq, nil
}

// bumpLastSeq advances runs.last_seq to seq. A missing runs row returns
// ErrRunNotFound (the application-level guard that keeps events from becoming
// orphans, on top of the FK).
func bumpLastSeq(ctx context.Context, tx txExecer, runID string, seq uint64) error {
	res, err := tx.ExecContext(ctx,
		"UPDATE runs SET last_seq=?, updated_at=? WHERE id=?",
		seq, nowMS(), runID)
	if err != nil {
		return fmt.Errorf("storage: update runs.last_seq: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrRunNotFound, runID)
	}
	return nil
}

// IsUniqueViolation reports whether err is a UNIQUE/PK constraint violation.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// newEventID generates a uuid v4 style event id.
func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure falls back to a time combination, which still
		// keeps the uniqueness trend.
		return fmt.Sprintf("evt-%d-%d", nowMS(), time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func nowMS() int64 { return time.Now().UnixMilli() }
