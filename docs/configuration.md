# Configuration

## Bot commands

Send these as plain text messages in any conversation with the bot:

| Command | Description |
|---|---|
| `!help` | Show available commands |
| `!restart` | Start a fresh session (discards context). Unlike a service restart, which resumes the on-disk session. |
| `!stop` | Abort the currently running agent turn |
| `!compact` | Compact conversation context to reduce token usage |
| `!skills` | List the skills loaded for this bot instance |
| `!verify` | Set up cross-signing so the bot's device shows as verified |

## General configuration

| Variable | Default | Description |
|---|---|---|
| `OPENCROW_PI_BINARY` | `pi` | Path to the pi binary |
| `OPENCROW_PI_SESSION_DIR` | `/var/lib/opencrow/sessions` | Session data directory |
| `OPENCROW_PI_PROVIDER` | `anthropic` | LLM provider |
| `OPENCROW_PI_MODEL` | `claude-opus-4-6` | Model name |
| `OPENCROW_PI_WORKING_DIR` | `/var/lib/opencrow` | Working directory for pi |
| `OPENCROW_PI_IDLE_TIMEOUT` | `30m` | Kill pi after this duration of inactivity |
| `OPENCROW_PI_SYSTEM_PROMPT` | built-in | Custom system prompt |
| `OPENCROW_SOUL_FILE` | _(empty)_ | Path to a file containing the system prompt (overrides `OPENCROW_PI_SYSTEM_PROMPT`) |
| `OPENCROW_PI_SKILLS` | _(empty)_ | Comma-separated skill directory paths |
| `OPENCROW_PI_SKILLS_DIR` | _(empty)_ | Directory containing skill subdirectories |
| `OPENCROW_SHOW_TOOL_CALLS` | `false` | Show tool invocations (bash, read, edit, …) as messages in the chat |
| `OPENCROW_DEBUG_TIMING` | `false` | Append task duration to each reply (useful for profiling local models) |

## File handling

**Receiving files** — Users can send images, audio, video, and documents to the
bot. Attachments are downloaded to the session directory under `attachments/`
and the file path is passed to pi so it can read or process the file with its
tools.

**Sending files back** — Pi can send files to the user by including
`<sendfile>/absolute/path</sendfile>` tags in its response. The bot strips the
tags, uploads each referenced file to Matrix, and delivers it as an attachment.
Multiple `<sendfile>` tags can appear in a single response.

## Matrix configuration

| Variable | Required | Description |
|---|---|---|
| `OPENCROW_MATRIX_HOMESERVER` | Yes | Matrix homeserver URL |
| `OPENCROW_MATRIX_USER_ID` | Yes | Bot's Matrix user ID |
| `OPENCROW_MATRIX_ACCESS_TOKEN` | Yes | Access token (via environment file) |
| `OPENCROW_MATRIX_DEVICE_ID` | No | Device ID (auto-resolved if omitted) |
| `OPENCROW_MATRIX_PICKLE_KEY` | No | Pickle key for crypto DB |
| `OPENCROW_MATRIX_CRYPTO_DB` | No | Path to crypto SQLite DB |
| `OPENCROW_MATRIX_ROOM_ID` | No | Default Matrix room ID for triggers, heartbeats, and reminders. When set, Matrix invite handling also switches to multi-room mode. |
| `OPENCROW_ALLOWED_USERS` | No | Comma-separated Matrix user IDs allowed to interact |

### Matrix room behavior

By default, Matrix runs in **single-room mode**: the bot joins the first
allowed room it is invited to and ignores later invites.

If `OPENCROW_MATRIX_ROOM_ID` is set, Matrix switches to **multi-room mode**:

two things happen:

1. triggers, heartbeats, and reminders are routed to that room by default
2. the bot accepts all allowed Matrix invites instead of claiming just one room

This does **not** create separate sessions per room. The bot still runs as one
shared agent with one shared session and conversation context across all rooms and
DMs.
Commands like `!restart`, `!stop`, and `!compact` still act on that shared
agent.

## Secrets and authentication

### LLM provider credentials

Pi needs credentials for your LLM provider. There are two ways to set this up:

**Option A: API key** — set `ANTHROPIC_API_KEY` (or the equivalent for your
provider) in an environment file and pass it via the `environmentFiles` option.
API keys don't expire and are the simplest approach.

**Option B: OAuth (Claude Pro/Max)** — pi supports OAuth against your Anthropic
account, so you can use your subscription instead of API credits. The NixOS
module installs an `opencrow-pi` wrapper on the host that runs pi inside the
container with the correct environment. To authenticate:

```bash
sudo opencrow-pi auth login
```

Pi will print a URL — open it in any browser, complete the Anthropic login, and
paste the redirect URL back into the terminal. No local browser is required on
the server itself. The refresh token persists across restarts — you only need to
do this once (unless the token gets revoked).

### Environment files

For secrets that are plain key=value pairs (e.g. API keys, access tokens), use
`environmentFiles`. These are bind-mounted read-only into the container and
loaded by systemd's `EnvironmentFile=` directive before the service starts:

```nix
services.opencrow.environmentFiles = [
  /run/secrets/opencrow-env  # contains ANTHROPIC_API_KEY=sk-...
];
```
