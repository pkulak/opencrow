# Heartbeat & Reminders

OpenCrow has two scheduling primitives: a **heartbeat** for periodic
awareness and **reminders** for scheduled prompts. One-shot and recurring
reminders share a 1-minute ticker.

## Heartbeat

Set `OPENCROW_HEARTBEAT_INTERVAL` to a Go duration (e.g. `30m`, `1h`). On
each tick the scheduler reads `<working-dir>/HEARTBEAT.md`, extracts active
checklist items, and sends them to the agent:

```md
# Standing checks
- Check for urgent email
- Review calendar for events in next 2h
- [paused] Weekly report draft
```

Only `- text` lines count. `- [paused] …` is skipped. No other metadata —
obsolete checks are deleted, not marked done. The agent can edit the file
at runtime to add or remove checks.

If the file has no active items, the tick is skipped (no API call). If the
agent replies `HEARTBEAT_OK` the response is suppressed; anything else is
delivered to the conversation.

Heartbeat prompts do not reset the idle timer — if no real user messages
arrive, the pi process is still reaped after the idle timeout.

### Enabling on NixOS

```nix
services.opencrow.environment.OPENCROW_HEARTBEAT_INTERVAL = "30m";
```

The reminder dispatcher runs regardless; this only controls the
HEARTBEAT.md checklist loop.

## Reminders

Enable the bundled `reminders` pi extension to give the agent structured
tools:

- `remind_at(when, prompt)` — schedule a one-shot reminder
- `remind_cron(cron, timezone, prompt, end_at?)` — schedule a recurring reminder
- `remind_list()` — list pending one-shot and recurring reminders
- `remind_cancel(id)` — cancel a one-shot reminder
- `remind_cron_cancel(id)` — cancel a recurring reminder

One-shot timestamps and optional recurring end times use ISO 8601 with an
explicit timezone. Recurring reminders use a five-field cron expression and
an explicit IANA timezone:

```text
cron:     0 12 * * 1
timezone: America/Los_Angeles
```

This fires every Monday at noon Pacific time. Cron uses the usual
`minute hour day-of-month month day-of-week` fields. When both day fields are
restricted, either one can match.

Every minute the scheduler deletes and enqueues due one-shot reminders, then
checks recurring schedules against the current minute. Recurring reminders
are deliberately loose scheduling: missed or failed occurrences are not
retried, and downtime does not produce catch-up reminders. If the inbox
already contains five items, matching recurring occurrences are skipped so
they cannot build an unbounded backlog.

Canceling or reaching the optional inclusive end time deletes a recurring
series. An occurrence already queued when the series is canceled may still
run.

`OPENCROW_SESSION_DIR` is exported into pi's environment automatically.

### Enabling on NixOS

```nix
services.opencrow.extensions.reminders = true;
```

This pulls the flake's `extension-reminders` package, which bakes the
`sqlite3` store path into the extension. For non-Nix installs the
extension falls back to PATH lookup, so make sure `sqlite3` is available
there.

## Trigger pipe

External processes (cron jobs, mail watchers, webhooks) can wake the bot
immediately by writing to the session directory's named pipe:

```text
<session-dir>/trigger.pipe
```

Each line written is processed as a separate trigger, delivered
immediately without waiting for a tick.

> [!CAUTION]
> The trigger pipe is an **unauthenticated** input channel. Any process
> that can write to the FIFO can inject arbitrary prompts into `pi`, which
> has full tool access. The FIFO is created with mode `0664`, so any
> process in the `opencrow` group can write to it. Make sure only trusted
> services are members of that group.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `OPENCROW_HEARTBEAT_INTERVAL` | _(empty, disabled)_ | How often to run through HEARTBEAT.md (Go duration) |
| `OPENCROW_HEARTBEAT_PROMPT` | built-in | Preamble sent before the checklist items |
