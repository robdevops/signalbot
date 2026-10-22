# signalbot

A small chat bot that connects to a running [signal-cli](https://github.com/AsamK/signal-cli) daemon over its socket and answers using the x.ai (Grok) API.

## Features

- Replies when it is @mentioned in a group; people it mentions are shown to the model by real name, and @names in its replies become real Signal mentions
- Replies when someone replies to one of its messages (the quoted text is passed as context)
- Replies to DMs from senders listed in `SIGNAL_ALLOWED_SENDERS`; ignores everything else
- Short answers sized for Signal (aims for one line, never over Signal's 2000-character limit); optional x.ai web search and configurable reasoning effort
- Short per-conversation memory, so follow-up questions work; the same message repeated within 10 seconds is ignored
- Shows a typing indicator (refreshed every 10s) while it thinks; many chats are handled at once and a slow answer never blocks others
- Logs who mentioned, replied to or messaged it, plus the token count and cost of each reply; reconnects automatically if signal-cli restarts

## Install

1. Start the signal-cli daemon (account already registered or linked):
   ```
   signal-cli -a +1234567890 daemon --socket /run/signal-cli/socket
   ```
   Or listen on TCP with `--tcp 127.0.0.1:7583` and set `SIGNALCLI_SOCKET=127.0.0.1:7583`.
2. Configure the bot:
   ```
   git clone https://github.com/robdevops/signalbot.git
   cd signalbot
   cp .env.example .env
   ```
   Edit `.env` and set `XAI_API_KEY` and `SIGNAL_ALLOWED_SENDERS`.
3. Install Go 1.24+ (skip for Docker):
   ```
   curl -fsSLO https://go.dev/dl/go1.24.7.linux-amd64.tar.gz
   sudo rm -rf /usr/local/go
   sudo tar -C /usr/local -xzf go1.24.7.linux-amd64.tar.gz
   ```
4. Build and run:
   ```
   /usr/local/go/bin/go build
   set -a; . ./.env; set +a
   ./signalbot
   ```
5. Or run as a systemd service. The bot runs as user `signalbot` in the `signal-cli` group, so the socket must be writable, and its directory searchable, by that group (check with `ls -ld /run/signal-cli /run/signal-cli/socket`):
   ```
   sudo useradd --system --no-create-home --shell /usr/sbin/nologin signalbot
   sudo install -m 755 signalbot /usr/local/bin/signalbot
   sudo install -m 600 .env /etc/signalbot.env
   sudo cp signalbot.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now signalbot
   ```
6. Or run in Docker. `--group-add` gives the container the `signal-cli` group, so it can reach the socket:
   ```
   docker build --build-arg VERSION=$(git describe --tags --always --dirty) -t signalbot .
   docker run -d --restart=always --env-file .env -v /run/signal-cli:/run/signal-cli \
     --group-add "$(getent group signal-cli | cut -d: -f3)" \
     --read-only --cap-drop=ALL --security-opt=no-new-privileges --pids-limit=64 --memory=300m \
     signalbot
   ```
7. Add the bot's account to a group and @mention it, reply to one of its messages, or DM it from an allowed sender.

## Tech specs

- Go, standard library only; single static binary
- Talks JSON-RPC to `signal-cli daemon` over a unix socket or TCP (`listAccounts` once per connection to learn its own id)
- x.ai Responses API (`/v1/responses`) over a shared keep-alive HTTP client, one retry on 429/5xx
- Bounded under load: a concurrency slot (`MAX_CONCURRENCY`) is taken before any goroutine starts, so messages it ignores cost nothing and a flood is dropped, not queued
- In-memory state only, all capped: history per chat (`HISTORY_TURNS`), number of chats (`MAX_CHATS`), names, frame size (1 MiB) and reply length (4 messages)
- Treats sender-controlled text as untrusted: display names and quotes are sanitised before reaching the model or the log, a quote is only called the bot's own when it matches a message the bot sent, and a name two people claim is never linked
- Configured from environment variables only; runs under systemd (hardened unit included) or Docker; log timestamps only when run from a terminal, since journald and Docker add their own

## Configuration

| Variable | Default | Description |
|---|---|---|
| `SIGNALCLI_SOCKET` | required | Unix socket path, or `host:port` for TCP |
| `XAI_API_KEY` | required | x.ai API key |
| `MODEL` | `grok-4.3` | Model name |
| `REASONING` | `low` | Reasoning effort (`none` to omit) |
| `XAI_SEARCH` | `true` | Enable x.ai web search tool (`true`/`false`, `yes`/`no`, `on`/`off`, `1`/`0`) |
| `SIGNAL_ALLOWED_SENDERS` | empty | UUIDs/numbers allowed to DM the bot, separated by commas or whitespace |
| `SYSTEM_PROMPT` | empty | Extra instructions, appended to the built-in prompt (which keeps answers short and Signal-sized) |
| `LOG_MESSAGE_BODY` | `false` | Also log the text of each handled message, not just who sent it |
| `BOT_NAME` | profile name | Name the bot gives when asked to introduce itself; falls back to `signalbot` if no profile name is found |
| `INTRO` | built-in sentence | Overrides the self-introduction; `{name}` and `{model}` are substituted |
| `XAI_PRICE_IN` | `1.25` | USD per million input tokens, for the cost estimate |
| `XAI_PRICE_CACHED` | `0.20` | USD per million cached input tokens, for the cost estimate |
| `XAI_PRICE_OUT` | `2.50` | USD per million output tokens, for the cost estimate |
| `MAX_CONCURRENCY` | `32` | Replies in flight at once; messages beyond this are dropped |
| `HISTORY_TURNS` | `10` | Exchanges remembered per chat |
| `MAX_CHATS` | `500` | Chats remembered before the least recently used is forgotten |
| `DEDUPE_SECONDS` | `10` | Ignore the same message from the same sender within this many seconds |

The default `INTRO` is: `hi, I'm {name}, your open source assistant running {model}. I respond to @mentions and replies to my messages. See https://github.com/robdevops/signalbot/blob/main/README.md for details`

The cost estimate uses xAI's published grok-4.3 rates, bills input served from xAI's prompt cache at the cached rate, and adds $5 per 1,000 web searches. It does not model long-prompt pricing tiers, so check [xAI's pricing](https://docs.x.ai/docs/models) and set the `XAI_PRICE_*` variables for other models.
