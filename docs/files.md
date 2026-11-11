# Files: send_file, list_attachments, fetch_attachment

The relay-hosted `slack` MCP server also carries acp-kit's MCP files tools
(the `io.modelcontextprotocol/files` extension; see acp-kit
[`docs/files-oob.md`](https://github.com/kfet/acp-kit/blob/main/docs/files-oob.md)).
They are registered whenever `agent_slack_access` is not `off`, and every
one is bound to the session's **own thread** — the thread is derived
server-side from the MCP connection token, so the agent cannot address
another one. They are listed only to MCP clients that declare the files
extension (fir ≥ v1.52.0).

| Tool | Needs | What happens |
| --- | --- | --- |
| `send_file` | nothing extra | Small files arrive inline and the relay uploads them (`files.getUploadURLExternal` → upload → `files.completeUploadExternal`). Larger files: the relay mints a pre-signed Slack `upload_url` with the bot token **server-side**, the agent's client POSTs the bytes straight to Slack, and the relay then shares the file into the thread. The upload URL needs no auth header, so the bot token never reaches the client. |
| `list_attachments` / `fetch_attachment` | `files_listen` + `files_public_base_url` | Lists files posted in the thread (newest first). `url_private` needs the bot token, so it is never handed out: `fetch_attachment` returns a one-shot link `<files_public_base_url>/dl/<token>` to the relay's download proxy, which fetches `url_private` with the bot token and streams it back. |

## Config

```json
{
  "max_send_file_bytes": 104857600,
  "files_listen": "unix:/run/slack-acp/files.sock",
  "files_public_base_url": "https://slack-acp.example.com"
}
```

- `max_send_file_bytes` — cap on one `send_file` (default 100 MiB; uploads
  above 2 GiB − 1 are refused regardless).
- `files_listen` — where the download proxy listens: `host:port`, or
  `unix:/path` for a socket fronted by a reverse proxy / agentbox Share.
- `files_public_base_url` — the public **https** URL that reaches
  `files_listen` (no credentials, query or fragment). Set both or neither.

The proxy only serves `GET /dl/<token>`. Each token is 128+ random bits,
single-use, valid for 10 minutes, and only minted for a file found in the
caller's own thread. Client headers are never forwarded; the bot token is
added only on the relay → Slack hop, only to `https://*.slack.com`, and redirects are not followed.
The proxy must be reachable over https only — terminate TLS in front of it.
A `unix:` socket is created with the process umask; make sure the fronting
proxy can connect to it.

## Guards

- `send_file` captions get the same treatment as `slack_post`: a caption
  starting with the self-drive sentinel is refused, the sentinel and
  channel-wide pings are scrubbed, and sends are capped by
  `agent_posts_per_minute` (a bucket of its own).
- Sending a file into the session's own thread is equivalent to its normal
  answer, so `send_file` is available under `read` as well as `read_write`.
- Scopes: `files:read` and `files:write` (already in the manifest).
