package main

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"strings"
	"unicode/utf8"
)

const (
	maxRoomContextEvents = 64
	maxRoomContextBytes  = 64 << 10
)

type roomContextEvent = RoomContext

type roomContextStore struct {
	db *sql.DB
}

func newRoomContextStore(db *sql.DB) *roomContextStore {
	return &roomContextStore{db: db}
}

// Append records a visible room event and keeps the pending context bounded.
func (s *roomContextStore) Append(ctx context.Context, event roomContextEvent) error {
	event = fitRoomContextEvent(event)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning room context append: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO room_context (
			conversation_id, message_id, speaker, worker, sender_name, sender_id, text
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, event.ConversationID, event.MessageID, event.Speaker, event.Worker,
		event.SenderName, event.SenderID, event.Text); err != nil {
		return fmt.Errorf("inserting room context: %w", err)
	}

	events, err := loadRoomContextEvents(ctx, tx, event.ConversationID)
	if err != nil {
		return err
	}

	existingDropped, err := loadRoomContextOmissions(ctx, tx, event.ConversationID)
	if err != nil {
		return err
	}

	drop := roomContextOverflow(events, existingDropped)
	if drop > 0 {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM room_context
			WHERE conversation_id = ? AND id IN (
				SELECT id FROM room_context
				WHERE conversation_id = ?
				ORDER BY id ASC
				LIMIT ?
			)
		`, event.ConversationID, event.ConversationID, drop); err != nil {
			return fmt.Errorf("pruning room context: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO room_context_omissions (conversation_id, dropped_count)
			VALUES (?, ?)
			ON CONFLICT(conversation_id) DO UPDATE
			SET dropped_count = dropped_count + excluded.dropped_count
		`, event.ConversationID, drop); err != nil {
			return fmt.Errorf("recording omitted room context: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing room context append: %w", err)
	}

	return nil
}

// EnqueueUser snapshots pending room context into a durable inbox item and
// consumes it in the same transaction. build receives the formatted context.
func (s *roomContextStore) EnqueueUser(
	ctx context.Context,
	params EnqueueInboxParams,
	replyToMessageID string,
	build func(string) string,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning contextual enqueue: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	events, err := loadRoomContextEvents(ctx, tx, params.ConversationID)
	if err != nil {
		return err
	}

	dropped, err := loadRoomContextOmissions(ctx, tx, params.ConversationID)
	if err != nil {
		return err
	}

	filtered := roomContextWithoutMessage(events, replyToMessageID)
	params.Content = build(formatRoomContext(filtered, dropped))

	if err := enqueueAndConsumeRoomContext(ctx, tx, params); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing contextual enqueue: %w", err)
	}

	return nil
}

func roomContextWithoutMessage(events []roomContextEvent, messageID string) []roomContextEvent {
	if messageID == "" {
		return events
	}

	filtered := events[:0]

	for _, event := range events {
		if event.MessageID != messageID {
			filtered = append(filtered, event)
		}
	}

	return filtered
}

func enqueueAndConsumeRoomContext(ctx context.Context, tx *sql.Tx, params EnqueueInboxParams) error {
	if err := New(tx).EnqueueInbox(ctx, params); err != nil {
		return fmt.Errorf("enqueuing contextual inbox item: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM room_context WHERE conversation_id = ?`, params.ConversationID); err != nil {
		return fmt.Errorf("consuming room context: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM room_context_omissions WHERE conversation_id = ?`, params.ConversationID); err != nil {
		return fmt.Errorf("clearing omitted room context count: %w", err)
	}

	return nil
}

func loadRoomContextEvents(ctx context.Context, q DBTX, conversationID string) ([]roomContextEvent, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, conversation_id, message_id, speaker, worker, sender_name, sender_id, text
		FROM room_context
		WHERE conversation_id = ?
		ORDER BY id ASC
	`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("reading room context: %w", err)
	}
	defer rows.Close()

	var events []roomContextEvent

	for rows.Next() {
		var event roomContextEvent

		if err := rows.Scan(
			&event.ID,
			&event.ConversationID,
			&event.MessageID,
			&event.Speaker,
			&event.Worker,
			&event.SenderName,
			&event.SenderID,
			&event.Text,
		); err != nil {
			return nil, fmt.Errorf("scanning room context: %w", err)
		}

		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating room context: %w", err)
	}

	return events, nil
}

func loadRoomContextOmissions(ctx context.Context, q DBTX, conversationID string) (int64, error) {
	var dropped int64

	err := q.QueryRowContext(ctx, `
		SELECT dropped_count FROM room_context_omissions WHERE conversation_id = ?
	`, conversationID).Scan(&dropped)
	if err == sql.ErrNoRows {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("reading omitted room context count: %w", err)
	}

	return dropped, nil
}

func roomContextOverflow(events []roomContextEvent, existingDropped int64) int {
	drop := max(0, len(events)-maxRoomContextEvents)
	for drop < len(events) && len(formatRoomContext(events[drop:], existingDropped+int64(drop))) > maxRoomContextBytes {
		drop++
	}

	return drop
}

func fitRoomContextEvent(event roomContextEvent) roomContextEvent {
	const (
		marker      = "\n[message truncated]"
		eventBudget = maxRoomContextBytes - 256 // leave room for the block and omission marker
	)

	for len(formatRoomContextEvent(event)) > eventBudget && event.Text != "" {
		overflow := len(formatRoomContextEvent(event)) - eventBudget
		keep := len(event.Text) - overflow - len(marker)

		if keep <= 0 {
			event.Text = "[message truncated]"

			break
		}

		for keep > 0 && !utf8.ValidString(event.Text[:keep]) {
			keep--
		}

		event.Text = event.Text[:keep] + marker
	}

	return event
}

func formatRoomContext(events []roomContextEvent, dropped int64) string {
	if len(events) == 0 && dropped == 0 {
		return ""
	}

	parts := make([]string, 0, len(events)+2)
	parts = append(parts, "<recent-room-messages>")

	if dropped > 0 {
		parts = append(parts, fmt.Sprintf("  <omitted-room-messages count=\"%d\" />", dropped))
	}

	for _, event := range events {
		parts = append(parts, formatRoomContextEvent(event))
	}

	parts = append(parts, "</recent-room-messages>")

	return strings.Join(parts, "\n")
}

func formatRoomContextEvent(event roomContextEvent) string {
	var attrs strings.Builder

	fmt.Fprintf(&attrs, ` speaker="%s"`, html.EscapeString(event.Speaker))

	if event.Worker != "" {
		fmt.Fprintf(&attrs, ` worker="%s"`, html.EscapeString(event.Worker))
	}

	if event.SenderName != "" {
		fmt.Fprintf(&attrs, ` sender-name="%s"`, html.EscapeString(event.SenderName))
	}

	if event.SenderID != "" {
		fmt.Fprintf(&attrs, ` sender-id="%s"`, html.EscapeString(event.SenderID))
	}

	if event.MessageID != "" {
		fmt.Fprintf(&attrs, ` message-id="%s"`, html.EscapeString(event.MessageID))
	}

	return fmt.Sprintf("  <room-message%s>\n    %s\n  </room-message>", attrs.String(), html.EscapeString(event.Text))
}
