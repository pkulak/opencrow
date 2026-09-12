# OpenCrow

A saner alternative to [OpenClaw](https://github.com/openclaw/openclaw).
<p align="center">
  <img src="logo.png" width="200" alt="OpenCrow logo">
</p>

OpenCrow is a Matrix bot that bridges chat messages to
[pi](https://github.com/badlogic/pi-mono), a coding agent with built-in tools,
session persistence, auto-compaction, and multi-provider LLM support. Instead of
reimplementing all of that in Go, OpenCrow spawns pi as a long-lived subprocess
via its RPC protocol and acts as a thin bridge. By default, the bot behaves as
a chat agent plus a separate background agent for reminders and external
triggers. Setting `OPENCROW_MATRIX_ROOM_ID` gives background work a stable
default room and enables multi-room invite handling.

```mermaid
graph LR
    Matrix -->|message| Inbox[(Inbox)]
    Reminders[(reminders)] -->|due| Inbox
    Trigger["trigger.pipe"] -->|external| Inbox
    Inbox -->|chat items| Chat["chat worker"] -->|RPC| ChatPi["chat pi"]
    Inbox -->|triggers| Background["background worker"] -->|RPC| BackgroundPi["background pi"]
    ChatPi -->|response| Matrix
    BackgroundPi -->|response| Matrix
```

The Go bot receives Matrix messages, forwards them to the pi process, collects
the response, and sends it back.

> [!WARNING]
> There is no whitelisting, permission system, or tool filtering. Trying to bolt
> that onto LLM tool use is inherently futile — the model will find a way around
> it. The only real protection is running OpenCrow in a containerized or sandboxed
> environment. **Use a NixOS container, VM, or similar isolation.** The included
> NixOS module does exactly that. Don't run it on a machine where you'd mind the
> LLM running arbitrary commands.

## Documentation

- **[Tutorial](docs/tutorial.md)** — Step-by-step NixOS deployment with Matrix
- **[Configuration](docs/configuration.md)** — Environment variables, Matrix settings, secrets, and authentication
- **[Skills](docs/skills.md)** — Teaching the agent new capabilities via markdown instructions
- **[Extensions](docs/extensions.md)** — TypeScript lifecycle hooks and custom tools
- **[Reminders](docs/reminders.md)** — One-shot reminders, recurring schedules, and trigger pipes
