package nostr

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	gonostr "fiatjaf.com/nostr"
)

const seenRumorsFile = ".nostr_seen_rumors"

// seenRumorsEntry is a single entry in the persisted seen-rumors file.
type seenRumorsEntry struct {
	ID   string `json:"id"`
	Seen int64  `json:"seen"` // unix timestamp of when we processed it
}

// loadSeenRumors reads persisted rumor IDs from disk, pruning entries older
// than maxAge. Returns a map of rumor ID hex → processing time.
func loadSeenRumors(baseDir string, maxAge time.Duration) map[string]time.Time {
	result := make(map[string]time.Time)

	data, err := os.ReadFile(filepath.Join(baseDir, seenRumorsFile))
	if err != nil {
		return result
	}

	var entries []seenRumorsEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.Warn("nostr: failed to parse seen_rumors", "error", err)
		return result
	}

	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		t := time.Unix(e.Seen, 0)
		if t.After(cutoff) {
			result[e.ID] = t
		}
	}

	return result
}

// saveSeenRumors atomically writes the seen rumor IDs to disk.
// It writes to a temporary file in the same directory and renames
// it into place so a crash mid-write cannot corrupt the file.
func saveSeenRumors(baseDir string, seen map[string]time.Time) {
	dir := filepath.Dir(filepath.Join(baseDir, seenRumorsFile))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("nostr: failed to create dir for seen_rumors", "error", err)
		return
	}

	entries := make([]seenRumorsEntry, 0, len(seen))
	for id, t := range seen {
		entries = append(entries, seenRumorsEntry{ID: id, Seen: t.Unix()})
	}

	data, err := json.Marshal(entries)
	if err != nil {
		slog.Warn("nostr: failed to marshal seen_rumors", "error", err)
		return
	}

	tmpFile, err := os.CreateTemp(dir, ".seen_rumors_*.tmp")
	if err != nil {
		slog.Warn("nostr: failed to create temp file for seen_rumors", "error", err)
		return
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		slog.Warn("nostr: failed to write seen_rumors temp file", "error", err)
		return
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		slog.Warn("nostr: failed to close seen_rumors temp file", "error", err)
		return
	}

	finalPath := filepath.Join(baseDir, seenRumorsFile)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		slog.Warn("nostr: failed to rename seen_rumors into place", "error", err)
	}
}

// sinceFromSeenRumors returns the oldest processing time in the set,
// minus a safety margin. If the set is empty, returns now - maxAge.
func sinceFromSeenRumors(seen map[string]time.Time, maxAge time.Duration) gonostr.Timestamp {
	oldest := time.Now()
	for _, t := range seen {
		if t.Before(oldest) {
			oldest = t
		}
	}

	// Go back maxAge from the oldest entry (or from now if empty)
	since := oldest.Add(-maxAge)
	ts := gonostr.Timestamp(since.Unix())
	if ts < 0 {
		ts = 0
	}
	return ts
}
