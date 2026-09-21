# Home Assistant voice assistant

OpenCrow can act as the conversation agent in a Home Assistant Assist pipeline.
Home Assistant handles the wake word, audio capture, speech-to-text, and
text-to-speech. OpenCrow receives one text request over HTTP, runs it through a
dedicated Pi session, and returns text for Home Assistant to speak.

```mermaid
graph LR
    Satellite["Voice satellite"] -->|audio| HA["Home Assistant Assist"]
    HA -->|POST /v1/turn| API["OpenCrow voice API"]
    API --> Voice["voice worker"] -->|RPC| Pi["voice Pi session"]
    Pi --> Voice -->|text| API -->|text| HA
    HA -->|audio| Satellite
```

The HTTP API is disabled by default. Enabling it adds a third worker alongside
chat and background. The voice worker has separate conversation context, but it
uses the normal provider, model, soul, working directory, skills, and tools.
There is no restricted voice permission profile.

The API is an addition to the Matrix bot, not a standalone OpenCrow backend.
Matrix configuration is still required, and Matrix is also used to administer
the voice session and receive files or messages intentionally routed from a
spoken request.

## Enable the HTTP endpoint

Set a listen address and provide a bearer token through an environment file:

```nix
services.opencrow = {
  environment.OPENCROW_HTTP_LISTEN = "0.0.0.0:8787";
  environmentFiles = [
    /run/secrets/opencrow-env
  ];
};

# Needed when Home Assistant connects directly through the host firewall.
networking.firewall.allowedTCPPorts = [ 8787 ];
```

The environment file should contain a long random token:

```text
OPENCROW_HTTP_BEARER_TOKEN=replace-with-a-random-secret
```

You can generate one with:

```bash
openssl rand -hex 32
```

OpenCrow refuses to start if `OPENCROW_HTTP_LISTEN` is set without
`OPENCROW_HTTP_BEARER_TOKEN`. Open the selected port in the host firewall only
for networks that need it.

Check the listener before configuring Home Assistant:

```bash
curl http://opencrow.example.test:8787/healthz
```

```json
{"status":"ok"}
```

`/healthz` is deliberately unauthenticated and only proves that the HTTP server
is running. Use the authenticated status endpoint to check the voice service:

```bash
export OPENCROW_TOKEN='replace-with-the-configured-token'

curl \
  -H "Authorization: Bearer $OPENCROW_TOKEN" \
  http://opencrow.example.test:8787/v1/status
```

```json
{
  "status": "ok",
  "ready": true,
  "active": false,
  "queue_depth": 0,
  "session_active": false
}
```

The status fields are:

| Field | Description |
|---|---|
| `status` | `ok` while the endpoint is responding |
| `ready` | `false` only while the voice session is being reset |
| `active` | Whether a voice turn is currently running |
| `queue_depth` | Number of voice turns waiting behind the active turn |
| `session_active` | Whether the voice Pi subprocess is currently alive |

`session_active` will be `false` before the first turn and after the normal Pi
idle timeout. That does not mean the endpoint is broken; the process starts
lazily on the next request.

## Install the Home Assistant integration

The repository includes a custom Home Assistant integration, but it is not
packaged for HACS. Install it manually from:

```text
integrations/home_assistant/custom_components/opencrow
```

Copy that directory into Home Assistant's configuration directory so the final
path is:

```text
/config/custom_components/opencrow
```

For example, when the repository is available on the Home Assistant host:

```bash
mkdir -p /config/custom_components
cp -a integrations/home_assistant/custom_components/opencrow \
  /config/custom_components/
```

Restart Home Assistant, then go to **Settings → Devices & services → Add
integration** and select **OpenCrow**. Enter:

- the base URL, including the scheme and port, such as
  `http://opencrow.example.test:8787`
- the bearer token configured on the OpenCrow server

The setup flow calls `GET /v1/status`, so bad credentials and unreachable
endpoints fail before the integration is saved.

The integration has been tested against Home Assistant 2026.5. It uses Home
Assistant's current conversation entity API; older releases may not load it.

## Configure the Assist pipeline

Edit the Assist pipeline used by your voice satellite and select **OpenCrow** as
the conversation agent. Leave **Prefer handling commands locally** enabled if
your Home Assistant version offers it. Home Assistant can then handle its built-in
intents directly and send conversational or tool-heavy requests to OpenCrow.

The rest of the pipeline is normal Home Assistant configuration:

- the satellite or ESPHome device handles the wake word and audio I/O
- Home Assistant performs speech-to-text
- OpenCrow receives only the resulting text
- Home Assistant performs text-to-speech on OpenCrow's response

OpenCrow does not accept audio and does not provide wake-word, STT, or TTS
services.

For each turn, the integration forwards the Home Assistant conversation ID,
device or satellite ID, area ID, language, and user ID when available. The area
lets OpenCrow interpret requests such as “turn on the lights in here.”

## Send a turn directly

`POST /v1/turn` runs one synchronous text turn. It requires a caller-generated
UUID so retries can be deduplicated.

```bash
curl \
  -H "Authorization: Bearer $OPENCROW_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "request_id": "35cc6ab3-12fe-4bc5-8dda-31f599d5ce78",
    "text": "Turn on the lights in here",
    "context": {
      "conversation_id": "01JEXAMPLE",
      "device_id": "voice_satellite_kitchen",
      "area_id": "kitchen",
      "language": "en",
      "user_id": "alice"
    }
  }' \
  http://opencrow.example.test:8787/v1/turn
```

A normal response is:

```json
{
  "request_id": "35cc6ab3-12fe-4bc5-8dda-31f599d5ce78",
  "text": "I turned on the kitchen lights.",
  "delivery": "voice"
}
```

### Request fields

| Field | Required | Description |
|---|---:|---|
| `request_id` | Yes | UUID used for cancellation, retry deduplication, and response correlation |
| `text` | Yes | Transcribed user request; leading and trailing whitespace is removed |
| `context` | No | Home Assistant metadata passed to the voice prompt |
| `context.conversation_id` | No | Home Assistant conversation identifier |
| `context.device_id` | No | Voice device or satellite identifier |
| `context.area_id` | No | Home Assistant area containing the device |
| `context.language` | No | Language selected by the Assist pipeline |
| `context.user_id` | No | Home Assistant user identifier |

Unknown JSON fields are rejected. The request body is limited to 64 KiB, `text`
to 16 KiB, and each context field to 1 KiB. Text and context values must be valid
UTF-8.

### Response fields

| Field | Description |
|---|---|
| `request_id` | The UUID from the request |
| `text` | Text Home Assistant should speak |
| `delivery` | `voice` for a spoken response, or `matrix` when the requested content was sent to Matrix instead |

All API errors use the same shape:

```json
{
  "error": {
    "code": "queue_full",
    "message": "OpenCrow is already handling too many voice requests."
  }
}
```

Common statuses are:

| Status | Meaning |
|---:|---|
| `400` | Invalid JSON, UUID, text, or context |
| `401` | Missing or incorrect bearer token |
| `409` | The UUID was already used for different request content |
| `413` | The body or utterance exceeds its size limit |
| `429` | One request is active and four more are already queued |
| `502` | Pi or the configured model provider failed the turn |
| `503` | The voice session is stopping or restarting |
| `504` | The turn exceeded its 90-second deadline |

A `429` response includes `Retry-After: 5`.

## Queue and retry behavior

Voice turns are serialized through one Pi process. OpenCrow accepts at most five
pending calls: one active turn and four queued turns. The 90-second deadline
starts when the HTTP request is accepted, so time spent waiting in the queue
counts against it.

Completed results stay in memory for five minutes. The in-memory call cache is
bounded to 256 records. Retrying the same UUID with identical text and context
waits for or returns the original result. Reusing that UUID with different
content returns `409 Conflict`.

If the last HTTP client waiting for a request disconnects, OpenCrow removes the
queued item when possible or cancels the active Pi turn. Voice queue rows are
also discarded at startup because their HTTP callers no longer exist. A spoken
command will therefore never be replayed after an OpenCrow restart.

## Matrix delivery from voice

Normal replies are returned only to Home Assistant. They are not mirrored into
Matrix.

The voice Pi session can still use OpenCrow's response control tags:

- `<send-to>ROOM_ID</send-to>` sends the remaining response and any files to the
  selected Matrix room. Home Assistant receives a short spoken acknowledgement
  instead.
- `<sendfile>/absolute/path</sendfile>` without `<send-to>` uploads the file to
  `OPENCROW_MATRIX_ROOM_ID` while the remaining response is spoken. If there is
  no default room, the spoken response reports that the file could not be sent.
- `<react>` tags are removed and ignored because an HTTP voice turn has no source
  Matrix event to react to.

For example, the model can return:

```text
<send-to>!family:matrix.example</send-to>
Here is the shopping list.
<sendfile>/var/lib/opencrow/shopping-list.txt</sendfile>
```

The room receives the message and file, while the voice device says, “I sent
that to chat.”

## Manage the voice session from Matrix

Voice text is always treated as an agent prompt. Administrative commands are
accepted through Matrix instead:

| Command | Description |
|---|---|
| `!voice-stop` | Abort the active voice turn; queued turns remain queued |
| `!voice-restart` | Abort pending turns, clear the queue, and start a fresh voice session on the next request |
| `!voice-compact` | Compact the active voice session and post the summary to Matrix |

A normal service restart resumes the voice session stored under the configured
Pi session directory. Use `!voice-restart` when you intentionally want to
discard that context.

## Security and privacy

Treat the bearer token as full access to the OpenCrow instance. The voice worker
inherits the normal tools and skills, and OpenCrow does not apply a second
permission layer based on `user_id`, `device_id`, or any other context field.
Anyone with the token can provide those values.

Keep the endpoint on a trusted network or put it behind an authenticated TLS
reverse proxy. Do not expose the plain HTTP listener directly to the internet;
the bearer token would be visible to anything that can observe the connection.

At info level, OpenCrow logs request IDs and the supplied device and area IDs,
but not the utterance. Full request text is logged at debug level. Choose the
log level with that privacy difference in mind.

## Troubleshooting

### Home Assistant cannot connect

Check the unauthenticated listener first:

```bash
curl http://opencrow.example.test:8787/healthz
```

If that fails, check `OPENCROW_HTTP_LISTEN`, the host firewall, and whether the
OpenCrow service restarted after the configuration change. If it succeeds, try
the authenticated `/v1/status` request to separate network problems from a bad
token.

### Requests are immediately rejected as busy

`429 Too Many Requests` means all five pending slots are occupied. Wait at least
the `Retry-After` interval. If an active turn is stuck, use `!voice-stop` from
Matrix; use `!voice-restart` if you also need to discard queued work and session
context.

### A requested file was not sent

`<sendfile>` without `<send-to>` needs `OPENCROW_MATRIX_ROOM_ID`. Also verify that
the bot is joined to the destination room and can upload files there.
