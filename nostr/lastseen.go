package nostr

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	gonostr "fiatjaf.com/nostr"
	"github.com/pinpox/opencrow/atomicfile"
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
func saveSeenRumors(baseDir string, seen map[string]time.Time) {
	entries := make([]seenRumorsEntry, 0, len(seen))
	for id, t := range seen {
		entries = append(entries, seenRumorsEntry{ID: id, Seen: t.Unix()})
	}

	data, err := json.Marshal(entries)
	if err != nil {
		slog.Warn("nostr: failed to marshal seen_rumors", "error", err)

		return
	}

	destPath := filepath.Join(baseDir, seenRumorsFile)
	if err := atomicfile.Write(destPath, data); err != nil {
		slog.Warn("nostr: failed to save seen_rumors", "error", err)
	}
}

// nip59SafetyMargin accounts for NIP-59's randomized created_at timestamps
// which may be up to 2 days in the past. We add an extra day of margin.
const nip59SafetyMargin = 3 * 24 * time.Hour

// sinceFromSeenRumors returns a subscription start time based on the newest
// processed rumor. We go back by nip59SafetyMargin from the newest entry to
// account for NIP-59's randomized created_at. If the set is empty, returns
// now minus the safety margin.
func sinceFromSeenRumors(seen map[string]time.Time) gonostr.Timestamp {
	newest := time.Time{}

	for _, t := range seen {
		if t.After(newest) {
			newest = t
		}
	}

	if newest.IsZero() {
		newest = time.Now()
	}

	since := newest.Add(-nip59SafetyMargin)

	return max(gonostr.Timestamp(since.Unix()), 0)
}
