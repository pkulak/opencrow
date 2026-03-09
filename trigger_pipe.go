package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// TriggerPipeReader reads lines from a named pipe (FIFO) and enqueues
// them into the inbox. It is a thin adapter — all scheduling and
// priority logic lives in the inbox/worker.
type TriggerPipeReader struct {
	inbox      *InboxStore
	sessionDir string
}

// NewTriggerPipeReader creates a new trigger pipe reader.
func NewTriggerPipeReader(inbox *InboxStore, sessionDir string) *TriggerPipeReader {
	return &TriggerPipeReader{
		inbox:      inbox,
		sessionDir: sessionDir,
	}
}

// Start begins reading from the trigger pipe. Blocks until ctx is cancelled.
func (t *TriggerPipeReader) Start(ctx context.Context) {
	pipePath := TriggerPipePath(t.sessionDir)

	if err := ensureFIFO(pipePath); err != nil {
		slog.Error("trigger: failed to ensure FIFO", "path", pipePath, "error", err)

		return
	}

	slog.Info("trigger pipe reader started", "path", pipePath)

	go t.readLoop(ctx, pipePath)
}

// readLoop reads lines from the named pipe and enqueues each as a trigger.
func (t *TriggerPipeReader) readLoop(ctx context.Context, pipePath string) {
	// Open with O_RDWR so the fd stays open even when writers close their end.
	f, err := os.OpenFile(pipePath, os.O_RDWR, 0)
	if err != nil {
		slog.Error("trigger: failed to open FIFO", "path", pipePath, "error", err)

		return
	}
	defer f.Close()

	// Watch for context cancellation and close the fd to unblock the scanner.
	stop := context.AfterFunc(ctx, func() { f.Close() })
	defer stop()

	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if ctx.Err() != nil {
			return
		}

		slog.Info("trigger: received", "content", line)

		if err := t.inbox.Enqueue(ctx, PriorityTrigger, "trigger", line, ""); err != nil {
			slog.Error("trigger: failed to enqueue", "error", err)
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		slog.Warn("trigger: scanner error", "error", err)
	}
}

// TriggerPipePath returns the path to the trigger FIFO.
func TriggerPipePath(sessionDir string) string {
	return filepath.Join(sessionDir, "trigger.pipe")
}

// ensureFIFO creates a named pipe at the given path if it doesn't exist.
// If a non-FIFO file exists at the path, it is replaced.
func ensureFIFO(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating FIFO parent dir: %w", err)
	}

	info, err := os.Lstat(path)
	if err == nil {
		// File exists — check if it's already a FIFO
		if info.Mode()&os.ModeNamedPipe != 0 {
			return nil
		}
		// Not a FIFO, remove it
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("removing non-FIFO at %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking FIFO at %s: %w", path, err)
	}

	if err := syscall.Mkfifo(path, 0o664); err != nil {
		return fmt.Errorf("creating FIFO at %s: %w", path, err)
	}

	return nil
}

func buildTriggerPrompt(basePrompt, content string) string {
	var prompt strings.Builder

	prompt.WriteString(basePrompt)
	prompt.WriteString("\n\n--- External trigger ---\n")
	prompt.WriteString(content)
	prompt.WriteString("\n--- end trigger ---")

	return prompt.String()
}
