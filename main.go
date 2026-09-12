package main

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/pinpox/opencrow/matrix"
	// Register the pure-Go SQLite driver.
	_ "modernc.org/sqlite"
)

const (
	opencrowDBFile     = "opencrow.db"
	legacyOutboxDBFile = "sent_messages.db"
)

//go:embed sqlc/schema.sql
var dbSchema string

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Fprintln(os.Stdout, versionString())
		os.Exit(0)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLogLevel(os.Getenv("OPENCROW_LOG_LEVEL")),
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return a
		},
	})))

	os.Exit(run())
}

func run() int {
	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("failed to load config", "error", err)

		return 1
	}

	slog.Info("matrix config loaded")

	if err := os.MkdirAll(cfg.Pi.StateDir, 0o750); err != nil {
		slog.Error("failed to create session directory", "error", err)

		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := openDB(ctx, cfg.Pi.StateDir)
	if err != nil {
		slog.Error("failed to open database", "error", err)

		return 1
	}
	defer db.Close()

	inbox, err := NewInboxStore(ctx, db)
	if err != nil {
		slog.Error("failed to initialize inbox", "error", err)

		return 1
	}

	b, worker, backgroundWorker, err := wireServices(ctx, cfg, db, inbox)
	if err != nil {
		slog.Error("failed to initialize services", "error", err)

		return 1
	}

	return runServices(ctx, b, worker, backgroundWorker, cancel)
}

// runServices starts the Matrix backend and workers, then shuts them down.
func runServices(ctx context.Context, b *matrix.Backend, worker, backgroundWorker *Worker, cancel context.CancelFunc) int {
	setupShutdown(b, cancel)

	workerDone := spawnWorker(ctx, worker)
	backgroundDone := spawnWorker(ctx, backgroundWorker)

	slog.Info("opencrow starting")

	exitCode := 0

	if err := b.Run(ctx); err != nil {
		if ctx.Err() == nil {
			slog.Error("matrix sync exited with error", "error", err)

			exitCode = 1
		} else {
			slog.Info("shutdown complete")
		}
	}

	// Matrix sync may have returned without a signal (error path); ensure
	// both workers see ctx.Done so the joins below cannot hang.
	cancel()
	<-workerDone
	<-backgroundDone

	_ = b.Close()

	return exitCode
}

// sqliteDSNParams are the connection parameters for modernc.org/sqlite.
// Note: modernc uses _pragma=name(value), not the mattn/go-sqlite3
// shorthand _name=value — the latter is silently ignored, leaving
// busy_timeout=0 and journal_mode=delete, which causes SQLITE_BUSY
// on concurrent writes. _txlock=immediate prevents deferred-lock
// upgrade deadlocks when two writers overlap under WAL.
const sqliteDSNParams = "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"

// openDB opens the shared database for inbox and outbox tables.
func openDB(ctx context.Context, sessionDir string) (*sql.DB, error) {
	dbPath := filepath.Join(sessionDir, opencrowDBFile)

	db, err := sql.Open("sqlite", dbPath+sqliteDSNParams)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if _, err := db.ExecContext(ctx, dbSchema); err != nil {
		db.Close()

		return nil, fmt.Errorf("migrating schema: %w", err)
	}

	// Migrate existing databases: add newer inbox columns if absent.
	if err := migrateInboxConversationID(ctx, db); err != nil {
		db.Close()

		return nil, fmt.Errorf("migrating inbox conversation_id: %w", err)
	}

	if err := migrateInboxMessageMetadata(ctx, db); err != nil {
		db.Close()

		return nil, fmt.Errorf("migrating inbox message metadata: %w", err)
	}

	if err := migrateLegacyOutbox(ctx, db, sessionDir); err != nil {
		slog.Warn("failed to migrate legacy sent_messages.db", "error", err)
	}

	return db, nil
}

// migrateInboxConversationID adds the conversation_id column to the inbox
// table if it is missing.
func migrateInboxConversationID(ctx context.Context, db *sql.DB) error {
	var colName string

	err := db.QueryRowContext(ctx, "SELECT name FROM pragma_table_info('inbox') WHERE name = 'conversation_id'").Scan(&colName)
	if err == nil {
		// Column already exists — nothing to do.
		return nil
	}

	slog.Info("migrating inbox: adding conversation_id column")

	if _, err := db.ExecContext(ctx, `ALTER TABLE inbox ADD COLUMN conversation_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("adding conversation_id column: %w", err)
	}

	return nil
}

func migrateInboxMessageMetadata(ctx context.Context, db *sql.DB) error {
	columns := []struct {
		name      string
		statement string
	}{
		{"message_id", `ALTER TABLE inbox ADD COLUMN message_id TEXT NOT NULL DEFAULT ''`},
		{"is_group", `ALTER TABLE inbox ADD COLUMN is_group BOOLEAN NOT NULL DEFAULT FALSE`},
	}

	for _, column := range columns {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('inbox') WHERE name = ?", column.name).Scan(&count); err != nil {
			return fmt.Errorf("checking %s column: %w", column.name, err)
		}

		if count != 0 {
			continue
		}

		slog.Info("migrating inbox: adding column", "column", column.name)

		if _, err := db.ExecContext(ctx, column.statement); err != nil {
			return fmt.Errorf("adding %s column: %w", column.name, err)
		}
	}

	return nil
}

func migrateLegacyOutbox(ctx context.Context, db *sql.DB, sessionDir string) error {
	legacyPath := filepath.Join(sessionDir, legacyOutboxDBFile)

	if _, err := os.Stat(legacyPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("checking legacy db: %w", err)
	}

	slog.Info("migrating legacy sent_messages.db into opencrow.db")

	if _, err := db.ExecContext(ctx, "ATTACH DATABASE ? AS legacy", legacyPath); err != nil {
		return fmt.Errorf("attaching legacy db: %w", err)
	}

	defer db.ExecContext(ctx, "DETACH DATABASE legacy") //nolint:errcheck // best-effort detach

	if _, err := db.ExecContext(ctx, `
		INSERT OR IGNORE INTO sent_messages (conversation_id, message_id, text)
		SELECT conversation_id, message_id, text FROM legacy.sent_messages
	`); err != nil {
		return fmt.Errorf("copying legacy rows: %w", err)
	}

	if err := os.Remove(legacyPath); err != nil {
		return fmt.Errorf("removing legacy db: %w", err)
	}

	_ = os.Remove(legacyPath + "-wal")
	_ = os.Remove(legacyPath + "-shm")

	slog.Info("legacy sent_messages.db migrated and removed")

	return nil
}

// wireServices creates the Matrix backend, app, and worker using two-phase init.
func wireServices(ctx context.Context, cfg *Config, db *sql.DB, inbox *InboxStore) (*matrix.Backend, *Worker, *Worker, error) {
	// Phase 1: create objects with nil cross-references.
	worker := NewWorker(inbox, cfg.Pi)
	backgroundWorker := NewBackgroundWorker(inbox, cfg.BackgroundPi, defaultTriggerPrompt)

	var app *App

	b, err := createMatrixBackend(cfg,
		func(ctx context.Context, msg matrix.Message) { app.HandleMessage(ctx, msg) },
		func(_ string) { worker.Restart() },
	)
	if err != nil {
		return nil, nil, nil, err
	}

	// Phase 2: wire cross-references.
	app = NewApp(b, worker, inbox, db)
	app.SetBackgroundWorker(backgroundWorker)

	if cfg.GroupTriggerRegex != nil {
		app.SetGroupTriggerRegex(cfg.GroupTriggerRegex)
	}

	worker.SetApp(app)
	worker.SetMatrix(b)
	backgroundWorker.SetApp(app)
	backgroundWorker.SetMatrix(b)

	worker.piCfg.SystemPrompt = app.systemPrompt(worker.piCfg.SystemPrompt)
	backgroundWorker.piCfg.SystemPrompt = app.systemPrompt(backgroundWorker.piCfg.SystemPrompt)

	go reminderLoop(ctx, backgroundWorker)

	startTriggerPipe(ctx, backgroundWorker, cfg.Pi.StateDir)
	worker.StartIdleReaper(ctx)
	backgroundWorker.StartIdleReaper(ctx)

	return b, worker, backgroundWorker, nil
}

// spawnWorker runs the worker loop in a goroutine and returns a channel
// that closes when it exits. main must join on this before returning:
// Worker.Run is the only path that calls stopPi on shutdown, and
// os.Exit otherwise races it, leaving pi (plus tool subprocesses)
// running. Pdeathsig in StartPi is the backstop, but graceful SIGTERM
// via stopPi is the intended path.
func spawnWorker(ctx context.Context, w *Worker) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		w.Run(ctx)
		close(done)
	}()

	return done
}

func setupShutdown(b *matrix.Backend, cancel context.CancelFunc) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		b.Stop()
		cancel()
	}()
}

func createMatrixBackend(cfg *Config, handler matrix.MessageHandler, onRoomCleanup func(string)) (*matrix.Backend, error) {
	matrixCfg := matrix.Config{
		Homeserver:     cfg.Matrix.Homeserver,
		UserID:         cfg.Matrix.UserID,
		AccessToken:    cfg.Matrix.AccessToken,
		DeviceID:       cfg.Matrix.DeviceID,
		AllowedUsers:   cfg.Matrix.AllowedUsers,
		PickleKey:      cfg.Matrix.PickleKey,
		CryptoDBPath:   cfg.Matrix.CryptoDBPath,
		SessionBaseDir: cfg.Pi.SessionDir,
		MultiRoom:      cfg.Pi.DefaultRoomID != "",
	}

	b, err := matrix.New(matrixCfg, handler)
	if err != nil {
		return nil, fmt.Errorf("creating matrix backend: %w", err)
	}

	b.SetRoomCleanupCallback(onRoomCleanup)

	return b, nil
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
