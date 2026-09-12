# Reminders

OpenCrow has one-shot and recurring reminders. Both are delivered through the background agent session, separately from chat, and use a one-minute scheduler.

Enable the bundled `reminders` Pi extension to give the agent structured tools:

- `remind_at(when, prompt)` — schedule a one-shot reminder
- `remind_cron(cron, timezone, prompt, end_at?)` — schedule a recurring reminder
- `remind_list()` — list pending one-shot and recurring reminders
- `remind_cancel(id)` — cancel a one-shot reminder
- `remind_cron_cancel(id)` — cancel a recurring reminder

Reminder prompts must be self-contained. The background session does not receive chat history.

One-shot timestamps and optional recurring end times use ISO 8601 with an explicit timezone. Recurring reminders use a five-field cron expression and an explicit IANA timezone:

```text
cron:     0 12 * * 1
timezone: America/Los_Angeles
```

This fires every Monday at noon Pacific time. Cron fields are `minute hour day-of-month month day-of-week`. When both day fields are restricted, either one can match.

Recurring reminders are deliberately loose scheduling. Missed or failed occurrences are not retried, and downtime does not produce catch-up reminders. Before a recurring occurrence is queued, OpenCrow counts queued background triggers; it skips the occurrence when there are already five. One-shot reminders and trigger-pipe events are not capped.

Canceling or reaching the optional inclusive end time deletes a recurring series. An occurrence already queued when the series is canceled may still run.

## Background session

Reminders and trigger-pipe events use a shared, resumable background Pi session. It is independent from the chat session but has the same working directory, tools, skills, and system prompt. Set these optional overrides to use a cheaper model for background work:

- `OPENCROW_BACKGROUND_PI_PROVIDER`
- `OPENCROW_BACKGROUND_PI_MODEL`

Each falls back to its `OPENCROW_PI_*` equivalent. Background tool calls and infrastructure errors are logged rather than sent to Matrix. Normal replies still go to Matrix; `NO_REPLY` remains silent.

Use `!background-stop` to abort the active background task, or `!background-restart` to discard its current session before the next task. Both leave chat and queued reminders alone.

## Trigger pipe

External processes can wake the background agent by writing to the session directory's named pipe:

```text
<session-dir>/trigger.pipe
```

Each line is a separate trigger. The pipe is unauthenticated: any process that can write to it can inject prompts into Pi, which has full tool access. The FIFO is mode `0664`, so make sure only trusted processes are in the `opencrow` group.

### Enabling on NixOS

```nix
services.opencrow.extensions.reminders = true;
```

This pulls the flake's `extension-reminders` package, which bakes the `sqlite3` store path into the extension. For non-Nix installs the extension falls back to PATH lookup, so make sure `sqlite3` is available there.
