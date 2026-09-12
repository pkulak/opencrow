package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// Priority levels are retained for compatibility with existing inbox rows.
const (
	PriorityUser    = 0
	PriorityTrigger = 1
)

//go:generate sqlc generate

// InboxStore is a persistent priority queue backed by SQLite.
type InboxStore struct {
	queries *Queries
}

// NewInboxStore wraps an existing database connection. The schema must
// already be applied (openDB handles this). Clears stale legacy heartbeat and
// compact items left over from a previous crash.
func NewInboxStore(ctx context.Context, db *sql.DB) (*InboxStore, error) {
	queries := New(db)

	// Legacy heartbeat and compact items have in-memory state that doesn't
	// survive a restart, so purge any left over from a previous run.
	if err := queries.DeleteStaleItems(ctx); err != nil {
		return nil, fmt.Errorf("clearing stale inbox items: %w", err)
	}

	return &InboxStore{queries: queries}, nil
}

// Enqueue inserts an item without Matrix event metadata.
func (s *InboxStore) Enqueue(ctx context.Context, priority int64, source, content, replyTo, conversationID string) error {
	return s.enqueue(ctx, EnqueueInboxParams{
		Priority:       priority,
		Source:         source,
		Content:        content,
		ReplyTo:        replyTo,
		ConversationID: conversationID,
	})
}

// EnqueueUser inserts a user message with the Matrix metadata needed for
// reactions and processing indicators.
func (s *InboxStore) EnqueueUser(ctx context.Context, content, replyTo, conversationID, messageID string, isGroup bool) error {
	return s.enqueue(ctx, EnqueueInboxParams{
		Priority:       PriorityUser,
		Source:         sourceUser,
		Content:        content,
		ReplyTo:        replyTo,
		ConversationID: conversationID,
		MessageID:      messageID,
		IsGroup:        isGroup,
	})
}

// DequeueChat removes and returns a chat item. Returns sql.ErrNoRows if none exist.
func (s *InboxStore) DequeueChat(ctx context.Context) (Inbox, error) {
	return s.queries.DequeueChatInbox(ctx)
}

// DequeueBackground removes and returns a background item. Returns sql.ErrNoRows if none exist.
func (s *InboxStore) DequeueBackground(ctx context.Context) (Inbox, error) {
	return s.queries.DequeueBackgroundInbox(ctx)
}

// Requeue re-inserts an interrupted item.
func (s *InboxStore) Requeue(ctx context.Context, item Inbox) error {
	if err := s.queries.EnqueueInbox(ctx, EnqueueInboxParams{
		Priority:       item.Priority,
		Source:         item.Source,
		Content:        item.Content,
		ReplyTo:        item.ReplyTo,
		ConversationID: item.ConversationID,
		MessageID:      item.MessageID,
		IsGroup:        item.IsGroup,
	}); err != nil {
		return fmt.Errorf("requeueing %s item: %w", item.Source, err)
	}

	return nil
}

// Count returns the number of items in the inbox.
func (s *InboxStore) Count(ctx context.Context) (int64, error) {
	return s.queries.CountInbox(ctx)
}

// CountBackground returns the number of queued trigger items.
func (s *InboxStore) CountBackground(ctx context.Context) (int64, error) {
	return s.queries.CountBackgroundInbox(ctx)
}

func (s *InboxStore) enqueue(ctx context.Context, params EnqueueInboxParams) error {
	if err := s.queries.EnqueueInbox(ctx, params); err != nil {
		return fmt.Errorf("enqueuing inbox item: %w", err)
	}

	slog.Info("inbox: enqueued", "source", params.Source, "priority", params.Priority)

	return nil
}
