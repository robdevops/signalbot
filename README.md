# signalbot

A small, [privacy-conscious](PRIVACY.md) chat bot that connects to a running [signal-cli](https://github.com/AsamK/signal-cli) daemon over its socket and answers using a model from [OpenRouter](https://openrouter.ai).

- [Features](#features)
- [Use cases](#use-cases)
- [Prerequisites](#prerequisites)
- [Install](#install)
  - [With systemd](#with-systemd)
  - [With Docker](#with-docker)
  - [Then](#then)
- [Configuration](#configuration)
  - [MCP tools](#mcp-tools)
  - [Scheduling](#scheduling)
  - [Logs](#logs)
- [Troubleshooting](#troubleshooting)

## Features

- Responds to @mentions, replies to its messages, and DMs from senders in `SIGNAL_DM_USER_IDS`
- Tools: web search, built-in line and pie charts, and your own [MCP](#mcp-tools) tools
- [`/schedule`](#scheduling) a message or command, once or repeating, in a DM or a group
- Understands images and stickers (if the configured model supports vision)
- Short per-conversation memory, for context. Frees from memory after 20 messages (HISTORY_TURNS), 24 hours, or when Signal's _disappearing messages_ timer expires, whichever comes first. Message content is never logged to disk.
- Typing indicators

## Use cases

- Your group's brain trust: ask it to explain things, check facts or settle debates
- Explain or translate any message or photo by replying to it and tagging the bot
- Connect MCP tools, data, and charting to your group chat
- Modify the bot to add your own /commands (see `/example`)
- A private assistant in your DMs, or just the fun or convenience of a sassy LLM in your Signal groups

The messages the bot answers, and the names in them, are sent to OpenRouter and on to the model's provider. See [TECHNICAL.md](TECHNICAL.md) and [PRIVACY.md](PRIVACY.md) for details on how it works.

## Prerequisites

A running [signal-cli](https://github.com/AsamK/signal-cli) daemon with your Signal account registered or linked. How you run it is up to you, but the bot needs only its socket:

- The socket is at `/run/signal-cli/socket` (or set `SIGNALCLI_SOCKET`).
- A Unix group named `signal-cli` can write to the socket and search its directory. The bot's systemd unit joins that group (`SupplementaryGroups=signal-cli`) and Docker does too (`--group-add`). Create it with `sudo groupadd --system signal-cli`.
- signal-cli runs with `UMask=0007`, which makes the socket `0770`. The default umask gives `0755`, which locks the group out. Check with `ls -l /run/signal-cli/socket`.
- signal-cli's data directory holds your account keys, so keep it at mode `0700` (`chmod 700 ~/.local/share/signal-cli`). The umask above makes new files in it group-accessible, so the directory's own mode is what keeps the bot out. Don't run the bot as signal-cli's user.

[`examples/signal-cli.service`](examples/signal-cli.service) is an example unit that does this. Edit `User=` and the path in `ExecStart`, then copy it to `/etc/systemd/system/`, and run `sudo systemctl daemon-reload` and `sudo systemctl enable --now signal-cli`.

To use TCP instead, run signal-cli with `--tcp 127.0.0.1:7583` and set `SIGNALCLI_SOCKET=127.0.0.1:7583`. But any local user can then connect and control or spy on the Signal account (PRIVACY RISK).

## Install

Get the code:

```
git clone https://github.com/robdevops/signalbot.git
cd signalbot
```

Create the config. The file holds your API key, so it is root-only; edit it, setting `OPENROUTER_API_KEY` and `SIGNAL_DM_USER_IDS`:

```
sudo install -m 660 .env.example /etc/signalbot.env
sudoedit /etc/signalbot.env
```

Then install the bot with systemd or with Docker.

### With systemd

The bot runs as a dynamic user (`DynamicUser=yes`) in the `signal-cli` Unix group, which is how it reaches the socket. Its schedules are kept in its state directory (`StateDirectory=signalbot`, which is `/var/lib/private/signalbot` with a dynamic user), which systemd creates and passes to it as `$STATE_DIRECTORY`.

1. Install Go 1.24+:
   ```
   curl -fsSLO https://go.dev/dl/go1.24.7.linux-amd64.tar.gz
   sudo rm -rf /usr/local/go
   sudo tar -C /usr/local -xzf go1.24.7.linux-amd64.tar.gz
   ```
2. Build and install:
   ```
   /usr/local/go/bin/go build -o signalbot ./cmd/signalbot
   sudo install -m 755 signalbot /usr/local/bin/signalbot
   sudo cp signalbot.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now signalbot
   ```
3. Read the logs (see [Logs](#logs)):
   ```
   journalctl -fu signalbot
   journalctl -u signalbot -p warning   # warnings and errors only
   ```

### With Docker

`--group-add` gives the container the `signal-cli` Unix group, which is how it reaches the socket. Run it with `sudo`, because Docker reads `--env-file` as the calling user and the file is root-only. Logs go to journald where available, otherwise to Docker's rotating `local` driver; `docker logs` reads both.

Schedules are saved in `/data` in the container (`STATE_DIRECTORY=/data`), bind-mounted from the same host directory systemd would use. The container runs as user 65532, which must own it, or the bot exits asking for this:

```
sudo mkdir -p /var/lib/private/signalbot
sudo chown 65532 /var/lib/private/signalbot
```

Use one install per host, systemd or Docker: the unit's dynamic user and the container's user 65532 can't share the directory.

```
sudo docker build --build-arg VERSION=$(git describe --tags --always --dirty) -t signalbot .
sudo docker run -d --name signalbot --restart=always --env-file /etc/signalbot.env \
  -v /run/signal-cli:/run/signal-cli -v /var/lib/private/signalbot:/data \
  --group-add "$(getent group signal-cli | cut -d: -f3)" \
  $(test -S /run/systemd/journal/socket && echo --log-driver=journald || echo --log-driver=local) \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges --pids-limit=64 --memory=300m --memory-swap=300m --ulimit core=0 \
  signalbot
```

Read the logs:

```
docker logs -f signalbot
journalctl CONTAINER_NAME=signalbot -p err   # warnings and errors only, where Docker logs to journald
```

Docker's journald driver records stderr at priority 3 (err) and stdout at 6 (info), so warnings and errors, which the bot writes to stderr, both show as errors there (see [Logs](#logs)).

### Then

Add the bot's account to a group and @mention it, reply to one of its messages, or DM it from an allowed sender. Send it `/example` to see what it can do; that reply never goes to the model.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `SIGNALCLI_SOCKET` | required | Unix socket path, or `host:port` for TCP |
| `OPENROUTER_API_KEY` | required | [OpenRouter](https://openrouter.ai) API key |
| `MODEL` | `deepseek/deepseek-v4.1-flash-1017` | OpenRouter model name, as `provider/model` |
| `MODEL_REASONING` | `low` | Reasoning effort, e.g. `low`, `medium`, `high` (`none` to omit) |
| `MODEL_SEARCH` | `true` | Lets the model search the web (OpenRouter's web search tool) when it decides to; searches are billed. Accepts `true`/`false`, `yes`/`no`, `on`/`off`, `1`/`0` |
| `SIGNAL_DM_USER_IDS` | empty | UUIDs/numbers allowed to DM the bot, separated by commas or whitespace |
| `MODEL_SYSTEM_PROMPT` | empty | Extra instructions, appended to the built-in prompt (which keeps answers short and Signal-sized) |
| `INTRO` | built-in sentence | Overrides the self-introduction; `{name}`, `{model}` (without any OpenRouter `provider/` prefix) and `{tools}` (the names of the tools, or `none`) are substituted |
| `MAX_CONCURRENCY` | `32` | Conversations in flight at once; messages beyond this are dropped |
| `HISTORY_TURNS` | `10` | Exchanges remembered per chat (`HISTORY_TURNS` × 2 messages) |
| `GROUP_CONTEXT` | `true` | Remember group messages not addressed to the bot as context, so "what do you think @bot" works without replying to the message; they are never logged |
| `MCP_CONFIG` | empty | Path of an MCP servers file; see [MCP tools](#mcp-tools) |
| `STATE_DIRECTORY` | working directory | Where `/schedule` saves `schedules.json`. Set by systemd's `StateDirectory=signalbot` (`/var/lib/private/signalbot`) and by the Docker image (`/data`, which must be writable by user 65532) |
| `TZ` | `UTC` in Docker, the host's under systemd | Time zone for `/schedule` times without an offset, e.g. `Australia/Sydney` |

`REASONING` and `SYSTEM_PROMPT` are now `MODEL_REASONING` and `MODEL_SYSTEM_PROMPT`; the old names are ignored. `LOG_LEVEL` is gone: every level is always written (see [Logs](#logs)).

The default `INTRO` is: `hi, I'm {name}, your open source assistant running {model} (tools: {tools}). I respond to @mentions and replies to my messages. See https://github.com/robdevops/signalbot/blob/main/README.md for details.`

The cost logged with each reply is the amount OpenRouter reports billing for it.

### MCP tools

The model can call tools from [MCP](https://modelcontextprotocol.io) servers, remote (`url`) or local (`command`). Anyone who can address the bot can make it call them, so choose what each server offers: `allowed_tools` offers only the tools listed, `blocked_tools` offers all but those, and without either every tool the server has is offered. Put the servers in a file, for example `/etc/signalbot-mcp.json`:

```json
{"mcpServers": {
  "weather": {
    "url": "https://example.com/mcp",
    "headers": {"Authorization": "Bearer …"},
    "allowed_tools": ["get_forecast", "get_alerts"],
    "chat_commands": {
      "forecast": {"tool": "get_forecast", "require_arg": true},
      "alerts": {"tool": "get_alerts"}
    }
  },
  "files": {"command": "uvx", "args": ["some-mcp-server"], "env": {"KEY": "value"}, "blocked_tools": ["delete_file"]}
}}
```

`chat_commands` turn a server's tools into slash commands: `/forecast sydney this weekend` asks the model, offered only that tool and made to call it. With `require_arg`, a bare `/forecast` gets a usage reply from the bot without asking the model.

Two optional fields shape how a server's tools are offered:

- `prompt` is a line added to the system prompt while the server's tools are offered, for example `"prompt": "Use the weather tools for any forecast, and give temperatures in °C."`. Without it, nothing is added: the server's own `instructions` (from its MCP handshake) are not used.
- `keywords` offers the server's tools only when a message contains one of the words, ignoring capitals and matching whole words only, and for the 2 replies after one that matched or used its tools. For example `"keywords": ["weather", "forecast", "rain"]`. Without it, the tools are always offered. A chat command always gets its tool.

Add `"disabled": true` to a server to leave it out without deleting it. A server that can't be reached at startup is logged and left out. A tool named in either list that the server doesn't have is a config error, so a typo can't leave a blocked tool offered. Local servers get only `PATH`, `HOME` and their `env`, never the bot's own environment.

- **systemd:** keep the file root-only, uncomment the `LoadCredential=` and `Environment=MCP_CONFIG=` lines in `signalbot.service`, and restart. systemd hands the file to the service alone.
- **Docker:** add `-v /etc/signalbot-mcp.json:/etc/signalbot-mcp.json:ro -e MCP_CONFIG=/etc/signalbot-mcp.json`. The image has no shell or runtimes, so only remote servers (`url`) work.
- **Local servers under systemd** usually need the hardening loosened with a drop-in (`sudo systemctl edit signalbot`), for example `MemoryDenyWriteExecute=no` for Node, and `CacheDirectory=signalbot` with `Environment=HOME=/var/cache/signalbot` for npx or uvx caches.

### Scheduling

In a DM with the bot, a sender in `SIGNAL_DM_USER_IDS` can schedule anything they could post themselves:

```
/schedule "<message or /command>" <ISO 8601 time> [every hour|day|week|month|quarter|year] [group name]
/schedule list            your schedules, numbered
/schedule del <number>    delete one
/schedule help
```

For example `/schedule "@signalbot what's the weather today?" 2030-01-31T07:30 every day Family`. At each run the text acts as if you had just posted it, in the DM or the named group: plain text is posted by the bot as written (with @Name turned into real mentions), text that @mentions the bot gets its answer, and a `/command` runs.

- The time is like `2030-01-31T09:00`, in the bot's time zone (`TZ`), or with an offset, like `2030-01-31T09:00+10:00`. A one-off in the past is refused; a repeating schedule that starts in the past first runs at its next time. A monthly schedule from the 31st runs on the last day of shorter months.
- The group name is the rest of the line, with no quotes needed. It is matched among the groups you share with the bot, ignoring capitals, emoji and extra spaces, so `family` finds "🏡 Family 🎉". No match, or more than one, is refused with your groups listed. A schedule keeps the group's ID, so it survives a rename; if you leave the group its runs are dropped until you delete it.
- `/timer` and `/reminder` work the same as `/schedule`.
- Straight and curly quotes both work. A schedule can't run `/schedule`, `/timer` or `/reminder`, so it can't make more schedules. The bot never answers its own posts, so a scheduled message that @mentions it gets exactly one answer.
- If the bot was down when a run was due, a repeating schedule skips it and a one-off up to an hour late still runs; an older one-off is dropped.
- Schedules are saved in `schedules.json` in `STATE_DIRECTORY` until deleted. See [PRIVACY.md](PRIVACY.md).

### Logs

Each line has a journald priority, and every priority is always written:

| Priority | Used for |
|---|---|
| `<2>` crit | errors that stop the bot, such as a bad config |
| `<3>` err | errors, such as a failed model request |
| `<4>` warning | warnings, such as a dropped message under load |
| `<5>` notice | startup, connecting and reconnecting |
| `<6>` info | each message answered ("received group chat" or "received dm", then the reply's time, tokens and cost), tool calls |
| `<7>` debug | detail, such as contact list loads |

Priorities 5–7 go to stdout and 2–4 to stderr. Under systemd the lines carry their `<N>` prefix, so `journalctl -p warning` filters by level. Under Docker the lines are plain, and only the stream tells levels apart. Message text is never logged.

## Troubleshooting

**Permission denied on the socket.** Whatever connects to `/run/signal-cli/socket` must be in the `signal-cli` Unix group, and the socket must be group-writable (`ls -l /run/signal-cli/socket` should show `srwxrwx---` and the group `signal-cli`). Common causes:

- A service started before the group existed. Restart it: `sudo systemctl restart signal-cli signalbot`.
- You added yourself to the group (`sudo usermod -aG signal-cli "$USER"`) to test the socket with `socat`. Your open shell keeps its old groups until you log out and back in, or run `newgrp signal-cli`.
- You run signal-cli from a shell instead of the systemd unit. It then uses your own group and umask, so the socket is `0755` and the group cannot write to it. Start it with the right group and umask, once `/run/signal-cli` exists with that group:

  ```
  # create socket directory:
  sudo install -d -o "$USER" -g signal-cli -m 0750 /run/signal-cli

  # run signal-cli
  sudo -u "$USER" -g signal-cli sh -c 'umask 007 && signal-cli -o json daemon --socket /run/signal-cli/socket --no-receive-stdout'
  ```

  On Linux, `sg signal-cli -c '…'` does the same without `sudo` if you are in the group. `doas` cannot set a group, so on OpenBSD use `su` or the system's `rc.d` service.
