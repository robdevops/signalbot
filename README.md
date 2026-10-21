# signalbot

A small chat bot that connects to a running `signal-cli` daemon over its socket and answers using the x.ai (Grok) API.

## Features

- Replies when it is @mentioned in a group
- Replies when someone replies to one of its messages (the quoted text is passed as context)
- Replies to DMs from senders listed in `SIGNAL_ALLOWED_SENDERS`; ignores everything else
- Optional x.ai web search and configurable reasoning effort
- Short per-conversation memory, so follow-up questions work
- Handles many chats at once; a slow answer never blocks other messages
- Reconnects automatically if signal-cli restarts

## Tech specs

- Go, standard library only; single static binary
- Talks JSON-RPC to `signal-cli daemon` over a unix socket or TCP (`listAccounts` once per connection to learn its own id)
- x.ai Responses API (`/v1/responses`) over a shared keep-alive HTTP client, one retry on 429/5xx
- One goroutine per accepted message, bounded by a semaphore (`MAX_CONCURRENCY`, default 32)
- In-memory history only (`HISTORY_TURNS`, default 10); no database or disk state
- Configuration from environment variables only; runs under systemd or Docker (distroless image)

## Configuration

| Variable | Default | Description |
|---|---|---|
| `SIGNALCLI_SOCKET` | required | Unix socket path, or `host:port` for TCP |
| `XAI_API_KEY` | required | x.ai API key |
| `MODEL` | `grok-4.3` | Model name |
| `REASONING` | `low` | Reasoning effort (`none` to omit) |
| `XAI_SEARCH` | `true` | Enable x.ai web search tool |
| `SIGNAL_ALLOWED_SENDERS` | empty | Comma-separated UUIDs/numbers allowed to DM the bot |

## Run

Start signal-cli: `signal-cli -a +1234567890 daemon --socket /run/signal-cli/socket` (or `--tcp 127.0.0.1:7583`).

systemd: `go build -o /usr/local/bin/signalbot .`, copy `.env.example` to `/etc/signalbot/env`, install `signalbot.service`, then `systemctl enable --now signalbot`.

Docker:

```
docker build -t signalbot .
docker run -d --restart=always --env-file .env -v /run/signal-cli:/run/signal-cli signalbot
```
