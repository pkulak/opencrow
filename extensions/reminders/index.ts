/**
 * Reminders Extension — scheduled prompts for opencrow
 *
 * Gives the LLM structured tools to manage one-shot reminders and recurring
 * cron series in opencrow.db. The Go-side scheduler polls both tables every
 * minute and delivers matching reminders as trigger messages.
 *
 * Tools:
 *   remind_at(when, prompt)                         — schedule a one-shot reminder
 *   remind_cron(cron, timezone, prompt, end_at?)   — schedule a recurring reminder
 *   remind_list()                                  — list active reminders
 *   remind_cancel(id)                              — cancel a one-shot reminder
 *   remind_cron_cancel(id)                         — cancel a recurring reminder
 *
 * The extension only writes to SQLite; all scheduling, delivery and cleanup
 * is owned by the opencrow process.
 */

import type { ExtensionAPI } from "@mariozechner/pi-coding-agent";
import { Type } from "@sinclair/typebox";

// Nix build substitutes the store path here. If the placeholder survives
// (non-Nix install), fall back to PATH lookup.
const SQLITE_BIN_RAW = "@@SQLITE_BIN@@";
const SQLITE_BIN = SQLITE_BIN_RAW.startsWith("@@") ? "sqlite3" : SQLITE_BIN_RAW;

const DB_PATH =
  process.env.OPENCROW_SESSION_DIR
    ? `${process.env.OPENCROW_SESSION_DIR}/opencrow.db`
    : undefined;

// Human-readable delta so the agent can sanity-check its own timezone
// math ("in 13h" when the user said "in an hour" is an obvious red flag).
function humanizeDelta(ms: number): string {
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  const rm = m % 60;
  if (h < 24) return rm ? `${h}h ${rm}m` : `${h}h`;
  const d = Math.floor(h / 24);
  const rh = h % 24;
  if (d < 7) return rh ? `${d}d ${rh}h` : `${d}d`;
  const w = Math.floor(d / 7);
  const rd = d % 7;
  return rd ? `${w}w ${rd}d` : `${w}w`;
}

// SQLite single-quote escaping: double the quote. Input is always wrapped
// in single quotes, so this is sufficient to prevent injection.
function q(s: string): string {
  return `'${s.replace(/'/g, "''")}'`;
}

// Normalize to ISO 8601 UTC so the scheduler's datetime() comparison
// matches. Rejects timestamps without an explicit offset: Date.parse
// treats "2025-06-15T14:00" as local to the *server*, not the user, and
// "2025-06-15" as UTC midnight — both silently wrong. Forcing the agent
// to be explicit avoids reminders firing hours off.
function normalizeWhen(when: string): string {
  if (!/(?:Z|[+-]\d{2}:?\d{2})$/.test(when.trim())) {
    throw new Error(
      `timestamp '${when}' has no timezone — append Z or an offset, ` +
        `e.g. 2025-06-15T14:00:00Z or 2025-06-15T14:00:00+02:00`,
    );
  }
  const ms = Date.parse(when);
  if (Number.isNaN(ms)) {
    throw new Error(
      `invalid timestamp '${when}' — use ISO 8601, e.g. 2025-06-15T14:00:00Z`,
    );
  }
  // A past timestamp would insert a row that fires on the very next tick
  // — almost certainly a timezone or arithmetic mistake on the agent's
  // part. Reject it here so the agent gets immediate feedback instead of
  // a confusing instant-fire.
  if (ms <= Date.now()) {
    throw new Error(
      `timestamp '${when}' is in the past — reminders must be in the future`,
    );
  }
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");
}

function normalizeCron(expression: string): string {
  const value = expression.trim().replace(/\s+/g, " ");
  if (value.split(" ").length !== 5) {
    throw new Error(
      `invalid cron expression '${expression}' — expected exactly five fields: ` +
        "minute hour day-of-month month day-of-week",
    );
  }
  return value;
}

function normalizeTimezone(timezone: string): string {
  const value = timezone.trim();
  if (!value || /^[+-]\d{2}:?\d{2}$/.test(value)) {
    throw new Error(
      `invalid timezone '${timezone}' — use an IANA name, e.g. America/Los_Angeles`,
    );
  }

  try {
    return new Intl.DateTimeFormat("en-US", { timeZone: value }).resolvedOptions().timeZone;
  } catch {
    throw new Error(
      `invalid timezone '${timezone}' — use an IANA name, e.g. America/Los_Angeles`,
    );
  }
}

export default function remindersExtension(pi: ExtensionAPI) {
  if (!DB_PATH) {
    // OPENCROW_SESSION_DIR is exported by opencrow's StartPi; if it is
    // missing we are running outside opencrow — silently skip.
    return;
  }

  async function sqlite(sql: string, signal?: AbortSignal): Promise<string> {
    const result = await pi.exec(
      SQLITE_BIN,
      // .timeout mirrors the Go side's busy_timeout(5000): WAL persists
      // in the file header but busy_timeout does not, so without this the
      // CLI fails SQLITE_BUSY instantly when racing the Go dispatcher.
      ["-batch", "-noheader", "-cmd", ".timeout 5000", DB_PATH, sql],
      { signal, timeout: 5000 },
    );
    if (result.code !== 0) {
      throw new Error(`sqlite3 failed: ${result.stderr || result.stdout}`);
    }
    return result.stdout.trim();
  }

  pi.registerTool({
    name: "remind_at",
    label: "Set reminder",
    description:
      "Schedule a one-shot reminder. The prompt is delivered to the separate " +
      "background agent at the given time (±1 min), then auto-deleted. Make " +
      "the prompt self-contained: it cannot see this chat's history.",
    parameters: Type.Object({
      when: Type.String({
        description:
          "Future ISO 8601 timestamp with explicit timezone, " +
          "e.g. 2025-06-15T14:00:00+02:00",
      }),
      prompt: Type.String({
        description: "Message to deliver when the reminder fires.",
      }),
    }),
    async execute(_id, params, signal) {
      const at = normalizeWhen(params.when);
      const delta = Date.parse(at) - Date.now();
      const out = await sqlite(
        `INSERT INTO reminders (fire_at, prompt) VALUES (${q(at)}, ${q(params.prompt)}); ` +
          `SELECT last_insert_rowid();`,
        signal,
      );
      return {
        content: [
          {
            type: "text",
            text: `Reminder #${out} set for ${at} — in ${humanizeDelta(delta)}`,
          },
        ],
        details: { id: Number(out), fire_at: at },
      };
    },
  });

  pi.registerTool({
    name: "remind_cron",
    label: "Set recurring reminder",
    description:
      "Schedule a recurring reminder using a five-field cron expression. The " +
      "prompt runs in a separate background session, so it must be self-contained. " +
      "Matching is checked once per minute in the supplied IANA timezone. " +
      "Missed or failed occurrences are not retried. Day-of-month and " +
      "day-of-week use standard cron OR semantics when both are restricted.",
    parameters: Type.Object({
      cron: Type.String({
        description:
          "Five-field cron expression: minute hour day-of-month month day-of-week, " +
          "e.g. '0 12 * * 1' for Mondays at noon.",
      }),
      timezone: Type.String({
        description: "IANA timezone name, e.g. America/Los_Angeles.",
      }),
      prompt: Type.String({
        description: "Message to deliver whenever the cron schedule matches.",
      }),
      end_at: Type.Optional(
        Type.String({
          description:
            "Optional inclusive end time as an ISO 8601 timestamp with explicit " +
            "timezone, e.g. 2026-12-31T23:59:00-08:00.",
        }),
      ),
    }),
    async execute(_id, params, signal) {
      const expression = normalizeCron(params.cron);
      const timezone = normalizeTimezone(params.timezone);
      const endAt = params.end_at ? normalizeWhen(params.end_at) : undefined;
      const out = await sqlite(
        `INSERT INTO recurring_reminders (cron, timezone, end_at, prompt) VALUES (` +
          `${q(expression)}, ${q(timezone)}, ${endAt ? q(endAt) : "NULL"}, ${q(params.prompt)}); ` +
          `SELECT last_insert_rowid();`,
        signal,
      );
      return {
        content: [
          {
            type: "text",
            text:
              `Recurring reminder #${out} set for '${expression}' in ${timezone}` +
              (endAt ? ` through ${endAt}.` : "."),
          },
        ],
        details: {
          id: Number(out),
          cron: expression,
          timezone,
          end_at: endAt,
        },
      };
    },
  });

  pi.registerTool({
    name: "remind_list",
    label: "List reminders",
    description: "List pending one-shot reminders and active recurring reminders.",
    parameters: Type.Object({}),
    async execute(_id, _params, signal) {
      const out = await sqlite(
        `SELECT 'one-shot #' || id || '  ' || fire_at || '  ' || prompt ` +
          `FROM reminders ORDER BY fire_at; ` +
          `SELECT 'recurring #' || id || '  ' || cron || '  [' || timezone || ']  ends ' || ` +
          `COALESCE(end_at, 'never') || '  ' || prompt FROM recurring_reminders ORDER BY id;`,
        signal,
      );
      return {
        content: [{ type: "text", text: out || "No reminders pending." }],
        details: {},
      };
    },
  });

  pi.registerTool({
    name: "remind_cancel",
    label: "Cancel reminder",
    description: "Delete a pending reminder by id.",
    parameters: Type.Object({
      id: Type.Integer({ description: "Reminder id to cancel" }),
    }),
    async execute(_id, params, signal) {
      const out = await sqlite(
        `DELETE FROM reminders WHERE id = ${params.id}; SELECT changes();`,
        signal,
      );
      const n = Number(out);
      return {
        content: [
          {
            type: "text",
            text: n > 0 ? `Reminder #${params.id} cancelled.` : `No reminder with id ${params.id}.`,
          },
        ],
        details: { deleted: n },
      };
    },
  });

  pi.registerTool({
    name: "remind_cron_cancel",
    label: "Cancel recurring reminder",
    description:
      "Delete an active recurring reminder series by id. An occurrence already " +
      "queued for delivery may still run.",
    parameters: Type.Object({
      id: Type.Integer({ description: "Recurring reminder series id to cancel" }),
    }),
    async execute(_id, params, signal) {
      const out = await sqlite(
        `DELETE FROM recurring_reminders WHERE id = ${params.id}; SELECT changes();`,
        signal,
      );
      const n = Number(out);
      return {
        content: [
          {
            type: "text",
            text:
              n > 0
                ? `Recurring reminder #${params.id} cancelled. Any occurrence already queued may still run.`
                : `No recurring reminder with id ${params.id}.`,
          },
        ],
        details: { deleted: n },
      };
    },
  });
}
