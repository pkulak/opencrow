package main

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// newTestDB creates an in-memory SQLite DB with the full schema applied.
func newTestDB(ctx context.Context, t *testing.T) *sql.DB {
	t.Helper()

	return newTestDBAt(ctx, t, ":memory:")
}

// newTestDBAt opens a SQLite DB at path (or ":memory:"), applies the schema
// and registers cleanup. Shared by inbox, outbox and trigger-pipe tests so
// the sql.Open + ExecContext(dbSchema) boilerplate lives in one place.
func newTestDBAt(ctx context.Context, t *testing.T, path string) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", path+sqliteDSNParams)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, dbSchema); err != nil {
		db.Close()
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	return db
}

// newTestInboxWithDB creates an InboxStore using an existing DB connection.
func newTestInboxWithDB(ctx context.Context, t *testing.T, db *sql.DB) *InboxStore {
	t.Helper()

	inbox, err := NewInboxStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	return inbox
}

// newTestInbox creates an InboxStore backed by an in-memory SQLite DB.
func newTestInbox(ctx context.Context, t *testing.T) *InboxStore {
	t.Helper()

	return newTestInboxWithDB(ctx, t, newTestDB(ctx, t))
}

func TestInbox_SourceRouting(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	inbox := newTestInbox(ctx, t)
	must(t, inbox.Enqueue(ctx, PriorityTrigger, sourceTrigger, "trigger", "", ""))
	must(t, inbox.Enqueue(ctx, PriorityUser, sourceUser, "user", "", ""))

	chat, err := inbox.DequeueChat(ctx)
	must(t, err)

	if chat.Source != sourceUser {
		t.Errorf("chat source = %q, want user", chat.Source)
	}

	background, err := inbox.DequeueBackground(ctx)
	must(t, err)

	if background.Source != sourceTrigger {
		t.Errorf("background source = %q, want trigger", background.Source)
	}
}

// seedInbox inserts rows directly into the inbox table, bypassing
// NewInboxStore cleanup, to simulate items left over from a crash.
func seedInbox(t *testing.T, db *sql.DB, rows []string) {
	t.Helper()

	ctx := context.Background()

	for _, q := range rows {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInbox_ClearsStaleItemsOnInit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := newTestDB(ctx, t)

	// Heartbeat and compact items have in-memory state that doesn't
	// survive a restart; both must be purged on init.
	seedInbox(t, db, []string{
		"INSERT INTO inbox (priority, source, content) VALUES (2, 'heartbeat', '')",
		"INSERT INTO inbox (priority, source, content) VALUES (0, 'compact', '')",
		// These should survive.
		"INSERT INTO inbox (priority, source, content) VALUES (0, 'user', 'keep me')",
		"INSERT INTO inbox (priority, source, content) VALUES (1, 'trigger', 'event data')",
	})

	inbox, err := NewInboxStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	count, err := inbox.Count(ctx)
	must(t, err)

	if count != 2 {
		t.Fatalf("count = %d, want 2 (heartbeat and compact should be cleared)", count)
	}

	item1, err := inbox.DequeueChat(ctx)
	must(t, err)

	if item1.Source != sourceUser {
		t.Errorf("first item source = %q, want %q", item1.Source, sourceUser)
	}

	item2, err := inbox.DequeueBackground(ctx)
	must(t, err)

	if item2.Source != sourceTrigger {
		t.Errorf("second item source = %q, want %q", item2.Source, sourceTrigger)
	}
}

func TestInbox_UserMetadataSurvivesRequeue(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	inbox := newTestInbox(ctx, t)

	must(t, inbox.EnqueueUser(ctx, "hello", "$reply", "!room", "$event", true))

	item, err := inbox.DequeueChat(ctx)
	must(t, err)

	if item.MessageID != "$event" || !item.IsGroup {
		t.Fatalf("user metadata = %+v", item)
	}

	must(t, inbox.Requeue(ctx, item))

	item, err = inbox.DequeueChat(ctx)
	must(t, err)

	if item.MessageID != "$event" || !item.IsGroup {
		t.Fatalf("requeued user metadata = %+v", item)
	}
}

func TestInbox_Persistence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := t.TempDir() + "/test.db"

	db1 := newTestDBAt(ctx, t, dbPath)
	inbox1 := newTestInboxWithDB(ctx, t, db1)

	must(t, inbox1.Enqueue(ctx, PriorityTrigger, sourceTrigger, "survived crash", "", ""))
	db1.Close()

	inbox2 := newTestInboxWithDB(ctx, t, newTestDBAt(ctx, t, dbPath))

	count, err := inbox2.Count(ctx)
	must(t, err)

	if count != 1 {
		t.Fatalf("count after reopen = %d, want 1", count)
	}

	item, err := inbox2.DequeueBackground(ctx)
	must(t, err)

	if item.Content != "survived crash" {
		t.Errorf("Content = %q, want %q", item.Content, "survived crash")
	}
}

func TestOpenDB_Pragmas(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	db, err := openDB(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var jm string
	must(t, db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&jm))

	if jm != "wal" {
		t.Errorf("journal_mode = %q, want wal", jm)
	}

	var bt int
	must(t, db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&bt))

	if bt != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", bt)
	}
}

func TestOpenDB_MigratesInboxColumns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()

	// Create a database with the old schema (no conversation_id column).
	dbPath := dir + "/opencrow.db"

	db, err := sql.Open("sqlite", dbPath+sqliteDSNParams)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS inbox (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			priority   INTEGER NOT NULL DEFAULT 2,
			source     TEXT    NOT NULL,
			content    TEXT    NOT NULL DEFAULT '',
			reply_to   TEXT    NOT NULL DEFAULT '',
			created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert a row with the old schema.
	_, err = db.ExecContext(ctx, "INSERT INTO inbox (priority, source, content) VALUES (0, 'user', 'old row')")
	if err != nil {
		t.Fatal(err)
	}

	db.Close()

	// Reopen via openDB, which should migrate the schema.
	db2, err := openDB(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	// Verify all added columns exist with backward-compatible defaults.
	var (
		conversationID string
		messageID      string
		isGroup        bool
	)

	err = db2.QueryRowContext(ctx, `
		SELECT conversation_id, message_id, is_group
		FROM inbox WHERE source = 'user'
	`).Scan(&conversationID, &messageID, &isGroup)
	if err != nil {
		t.Fatalf("failed to query migrated inbox columns: %v", err)
	}

	if conversationID != "" || messageID != "" || isGroup {
		t.Errorf("migrated defaults = (%q, %q, %v)", conversationID, messageID, isGroup)
	}
}

func must(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}
