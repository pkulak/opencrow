package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
)

// Priority levels for inbox items. Lower number = higher priority.
const (
	PriorityUser      = 0
	PriorityTrigger   = 1
	PriorityHeartbeat = 2
)

// InboxStore is a persistent priority queue backed by SQLite.
// Items survive crashes. A signal channel notifies the worker
// when new items are enqueued.
type InboxStore struct {
	queries *Queries
	db      *sql.DB

	// wake is signalled (non-blocking) on every enqueue so the worker
	// can poll the DB for the highest-priority item.
	wake chan struct{}

	// mu protects preempt state so the enqueue path can safely cancel
	// a running lower-priority operation.
	mu              sync.Mutex
	currentPriority int64 // priority of the item currently being processed
	currentCancel   context.CancelFunc
}

// NewInboxStore opens (or creates) the inbox database in the given directory.
// It reuses the same DB file as the sent message store since they share a schema.
func NewInboxStore(ctx context.Context, db *sql.DB) (*InboxStore, error) {
	if _, err := db.ExecContext(ctx, inboxSchema); err != nil {
		return nil, fmt.Errorf("migrating inbox table: %w", err)
	}

	queries := New(db)

	// Clear stale heartbeat markers from a previous run — the timer
	// will re-enqueue fresh ones.
	if err := queries.DeleteHeartbeatItems(ctx); err != nil {
		return nil, fmt.Errorf("clearing stale heartbeat items: %w", err)
	}

	return &InboxStore{
		queries:         queries,
		db:              db,
		wake:            make(chan struct{}, 1),
		currentPriority: -1, // nothing running
	}, nil
}

//go:generate sqlc generate

const inboxSchema = `CREATE TABLE IF NOT EXISTS inbox (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    priority   INTEGER NOT NULL DEFAULT 2,
    source     TEXT    NOT NULL,
    content    TEXT    NOT NULL DEFAULT '',
    reply_to   TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);`

// Enqueue inserts an item into the inbox and signals the worker.
// If the worker is currently processing a lower-priority item, its
// context is cancelled so it can pick up the new item.
func (s *InboxStore) Enqueue(ctx context.Context, priority int64, source, content, replyTo string) error {
	if err := s.queries.EnqueueInbox(ctx, EnqueueInboxParams{
		Priority: priority,
		Source:   source,
		Content:  content,
		ReplyTo:  replyTo,
	}); err != nil {
		return fmt.Errorf("enqueuing inbox item: %w", err)
	}

	slog.Info("inbox: enqueued", "source", source, "priority", priority)

	// Check if we should preempt the currently running operation.
	s.mu.Lock()
	if s.currentCancel != nil && priority < s.currentPriority {
		slog.Info("inbox: preempting current operation",
			"new_priority", priority,
			"current_priority", s.currentPriority,
		)
		s.currentCancel()
	}
	s.mu.Unlock()

	// Non-blocking signal to wake the worker.
	select {
	case s.wake <- struct{}{}:
	default:
	}

	return nil
}

// Dequeue removes and returns the highest-priority (lowest number) item.
// Returns sql.ErrNoRows if the inbox is empty.
func (s *InboxStore) Dequeue(ctx context.Context) (Inbox, error) {
	return s.queries.DequeueInbox(ctx)
}

// Wake returns the channel that is signalled when items are enqueued.
func (s *InboxStore) Wake() <-chan struct{} {
	return s.wake
}

// SetRunning records what the worker is currently processing so that
// Enqueue can preempt it if a higher-priority item arrives.
func (s *InboxStore) SetRunning(priority int64, cancel context.CancelFunc) {
	s.mu.Lock()
	s.currentPriority = priority
	s.currentCancel = cancel
	s.mu.Unlock()
}

// ClearRunning clears the current-operation state.
func (s *InboxStore) ClearRunning() {
	s.mu.Lock()
	s.currentPriority = -1
	s.currentCancel = nil
	s.mu.Unlock()
}

// Requeue re-inserts an item that was interrupted. Only triggers carry
// unique data worth preserving; heartbeat markers are dropped since the
// timer will fire again.
func (s *InboxStore) Requeue(ctx context.Context, item Inbox) {
	if item.Source == "heartbeat" {
		return // timer will re-enqueue
	}

	if err := s.queries.EnqueueInbox(ctx, EnqueueInboxParams{
		Priority: item.Priority,
		Source:   item.Source,
		Content:  item.Content,
		ReplyTo:  item.ReplyTo,
	}); err != nil {
		slog.Error("inbox: failed to requeue item", "source", item.Source, "error", err)
	}
}

// Count returns the number of items in the inbox.
func (s *InboxStore) Count(ctx context.Context) (int64, error) {
	return s.queries.CountInbox(ctx)
}
