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

See [TECHNICAL.md](TECHNICAL.md) for how it works.

## Prerequisites

[signal-cli](https://github.com/AsamK/signal-cli), installed at `/usr/local/bin/signal-cli`, running as its own user so that both install methods below can be granted access to its socket. Create the user and link your Signal account to it (this prints a link to scan in Signal's *Linked devices*; see the signal-cli docs to register a number instead):

```
sudo useradd --system --user-group --home-dir /var/lib/signal-cli --no-create-home --shell /usr/sbin/nologin signal-cli
sudo install -d -o signal-cli -g signal-cli -m 700 /var/lib/signal-cli
sudo -u signal-cli signal-cli --config /var/lib/signal-cli link --name signalbot
```

## Install

Get the code, and start signal-cli as a service. First set your account's number in `signal-cli.service` (the `-a +1234567890` in `ExecStart`):

```
git clone https://github.com/robdevops/signalbot.git
cd signalbot
sudo cp signal-cli.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now signal-cli
ls -l /run/signal-cli/socket
```

The socket should show as `srwxrwx--- signal-cli signal-cli`: group-writable, so members of the `signal-cli` group can connect. Both install methods below use that group. (To use TCP instead, run signal-cli with `--tcp 127.0.0.1:7583` and set `SIGNALCLI_SOCKET=127.0.0.1:7583`; then any local user can connect.)

Create the config. The file holds your API key, so it is root-only; edit it, setting `XAI_API_KEY` and `SIGNAL_ALLOWED_SENDERS`:

```
sudo install -m 660 .env.example /etc/signalbot.env
sudoedit /etc/signalbot.env
```

Then install the bot with systemd or with Docker.

### With systemd

The bot runs as user `signalbot` in the `signal-cli` group, which is how it reaches the socket.

1. Install Go 1.24+:
   ```
   curl -fsSLO https://go.dev/dl/go1.24.7.linux-amd64.tar.gz
   sudo rm -rf /usr/local/go
   sudo tar -C /usr/local -xzf go1.24.7.linux-amd64.tar.gz
   ```
2. Build and install:
   ```
   /usr/local/go/bin/go build
   sudo useradd --system --no-create-home --shell /usr/sbin/nologin signalbot
   sudo install -m 755 signalbot /usr/local/bin/signalbot
   sudo cp signalbot.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now signalbot
   ```

### With Docker

`--group-add` gives the container the `signal-cli` group, which is how it reaches the socket. Docker reads `--env-file` as the calling user, so run it with `sudo`: the file is root-only. Logs go to journald where the host has it, otherwise to Docker's rotating `local` driver; read them with `docker logs -f signalbot` either way:

```
sudo docker build --build-arg VERSION=$(git describe --tags --always --dirty) -t signalbot .
sudo docker run -d --name signalbot --restart=always --env-file /etc/signalbot.env -v /run/signal-cli:/run/signal-cli \
  --group-add "$(getent group signal-cli | cut -d: -f3)" \
  $(test -S /run/systemd/journal/socket && echo --log-driver=journald || echo --log-driver=local) \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges --pids-limit=64 --memory=300m \
  signalbot
```

### Then

Add the bot's account to a group and @mention it, reply to one of its messages, or DM it from an allowed sender.

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
| `INTRO` | built-in sentence | Overrides the self-introduction; `{name}` and `{model}` are substituted |
| `XAI_PRICE_IN` | `1.25` | USD per million input tokens, for the cost estimate |
| `XAI_PRICE_CACHED` | `0.20` | USD per million cached input tokens, for the cost estimate |
| `XAI_PRICE_OUT` | `2.50` | USD per million output tokens, for the cost estimate |
| `MAX_CONCURRENCY` | `32` | Replies in flight at once; messages beyond this are dropped |
| `HISTORY_TURNS` | `10` | Exchanges remembered per chat |

The default `INTRO` is: `hi, I'm {name}, your open source assistant running {model}. I respond to @mentions and replies to my messages. See https://github.com/robdevops/signalbot/blob/main/README.md for details`

The cost estimate uses xAI's published grok-4.3 rates, bills input served from xAI's prompt cache at the cached rate, and adds $5 per 1,000 web searches. It does not model long-prompt pricing tiers, so check [xAI's pricing](https://docs.x.ai/docs/models) and set the `XAI_PRICE_*` variables for other models.
