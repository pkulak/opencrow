-- name: UpsertOutbox :exec
INSERT INTO sent_messages (conversation_id, message_id, text)
VALUES (?, ?, ?)
ON CONFLICT(conversation_id, message_id) DO UPDATE SET text = excluded.text;

-- name: GetOutbox :one
SELECT text FROM sent_messages
WHERE conversation_id = ? AND message_id = ?;

-- name: CountOutbox :one
SELECT count(*) FROM sent_messages WHERE conversation_id = ?;

-- name: DeleteOldestOutbox :exec
DELETE FROM sent_messages
WHERE rowid IN (
    SELECT sm.rowid FROM sent_messages sm
    WHERE sm.conversation_id = ?
    ORDER BY sm.rowid ASC
    LIMIT ?
);

-- name: EnqueueInbox :exec
INSERT INTO inbox (
    priority, source, content, reply_to, conversation_id, message_id, is_group
)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: DequeueChatInbox :one
DELETE FROM inbox
WHERE id = (
    SELECT id FROM inbox
    WHERE source IN ('user', 'compact')
    ORDER BY priority ASC, id ASC
    LIMIT 1
)
RETURNING id, priority, source, content, reply_to, conversation_id,
          message_id, is_group, claimed_at, created_at;

-- name: ClaimBackgroundInbox :one
-- Claims the oldest pending trigger by stamping claimed_at. The row stays in
-- the inbox until the turn finishes so a crash or restart can recover it; see
-- ResetClaimedTriggers.
UPDATE inbox
SET claimed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = (
    SELECT id FROM inbox
    WHERE source = 'trigger' AND claimed_at = ''
    ORDER BY priority ASC, id ASC
    LIMIT 1
)
RETURNING id, priority, source, content, reply_to, conversation_id,
          message_id, is_group, claimed_at, created_at;

-- name: DequeueVoiceInbox :one
DELETE FROM inbox
WHERE id = (
    SELECT id FROM inbox
    WHERE source IN ('voice', 'voice_compact')
    ORDER BY priority ASC, id ASC
    LIMIT 1
)
RETURNING id, priority, source, content, reply_to, conversation_id,
          message_id, is_group, claimed_at, created_at;

-- name: CompleteInbox :execrows
DELETE FROM inbox WHERE id = ?;

-- name: ResetClaimedTriggers :execrows
-- A trigger is claimed while a background turn handles it. No turn can survive
-- a restart, so any claim left at startup belongs to a dead run and must be
-- retried.
UPDATE inbox SET claimed_at = '' WHERE source = 'trigger' AND claimed_at != '';

-- name: DeleteVoiceInbox :execrows
DELETE FROM inbox WHERE source = 'voice' AND message_id = ?;

-- name: DeleteAllVoiceInbox :exec
DELETE FROM inbox WHERE source IN ('voice', 'voice_compact');

-- name: DeleteStaleItems :exec
DELETE FROM inbox WHERE source IN ('heartbeat', 'compact', 'voice', 'voice_compact');

-- name: CountInbox :one
SELECT count(*) FROM inbox;

-- name: CountBackgroundInbox :one
SELECT count(*) FROM inbox WHERE source = 'trigger';

-- name: DueReminders :many
-- datetime() normalizes ISO 8601 variants (Z vs +00:00, T vs space) so
-- lexicographic comparison doesn't break on agent-formatted timestamps.
DELETE FROM reminders
WHERE datetime(fire_at) <= datetime(?)
RETURNING id, fire_at, prompt;

-- name: InsertReminder :exec
INSERT INTO reminders (fire_at, prompt) VALUES (?, ?);

-- name: ListRecurringReminders :many
SELECT id, cron, timezone, end_at, prompt
FROM recurring_reminders
ORDER BY id;

-- name: DeleteRecurringReminder :exec
DELETE FROM recurring_reminders WHERE id = ?;
