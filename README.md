# signalbot

A small, [privacy-conscious](PRIVACY.md) chat bot that connects to a running [signal-cli](https://github.com/AsamK/signal-cli) daemon over its socket and answers using a model from [OpenRouter](https://openrouter.ai) or [compatible provider](docs/PROVIDERS.md).

- [Features](#features)
- [Use cases](#use-cases)
- [Prerequisites](#prerequisites)
- [Install](#install)
  - [With systemd](#with-systemd)
  - [With Docker](#with-docker)
  - [Then](#then)
- [Configuration](#configuration)
- [Guides](#guides)

## Features

- Responds to @mentions, replies to its messages, and DMs from whitelisted contacts
- Tools: web search, line and pie charts, and your own [MCP](docs/MCP.md) tools
- [`/schedule`](docs/SCHEDULING.md) a message or command, once or repeating
- Understands [files](docs/FILES.md) (PDFs, text and code, Word, Excel and PowerPoint) and the [pages you link to](docs/LINKS.md), and if the model supports it, images, stickers, voice notes and other audio
- Typing indicators, reactions, stickers, @mentions and replies
- In-memory conversation context (Default: 10 messages). Never logged to disk, forgotten after 24 hours, or sooner if _disappearing messages_ requires.

## Use cases

- Your group's brain trust: ask it to explain things, check facts or settle debates
- Explain or translate any message, photo, document, or website by replying to it and tagging the bot
- Connect MCP tools, data, and charting to your group chat
- Modify the bot to add your own /commands
- A private assistant in your DMs, or just the fun of a sassy LLM in your Signal groups

The messages the bot answers, and the names in them, are sent to the model's API (OpenRouter, unless you [choose another](docs/PROVIDERS.md)). See [TECHNICAL.md](TECHNICAL.md) and [PRIVACY.md](PRIVACY.md) for details on how it works.

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

Create the config. The file holds your API key, so it is root-only; edit it, setting `MODEL_API_KEY` (your [OpenRouter](https://openrouter.ai) key; see [Other providers](docs/PROVIDERS.md) and [No model](docs/NO_MODEL.md) for the alternatives) and `SIGNAL_DM_USER_IDS`:

```
sudo install -m 660 .env.example /etc/signalbot.env
sudoedit /etc/signalbot.env
```

Then install the bot with systemd or with Docker.

### With systemd

The bot runs as a dynamic user (`DynamicUser=yes`) in the `signal-cli` Unix group, which is how it reaches the socket. Its schedules are kept in its state directory (`StateDirectory=signalbot`, which is `/var/lib/private/signalbot` with a dynamic user), which systemd creates and passes to it as `$STATE_DIRECTORY`. If the bot can't write there (for example, a copy of the unit without that line), it exits at startup asking for it.

1. Install Go 1.27+:
   ```
   curl -fsSLO https://go.dev/dl/go1.27.3.linux-amd64.tar.gz
   sudo rm -rf /usr/local/go
   sudo tar -C /usr/local -xzf go1.27.3.linux-amd64.tar.gz
   ```
2. Build and install:
   ```
   /usr/local/go/bin/go build -o signalbot
   sudo install -m 755 signalbot /usr/local/bin/signalbot
   sudo cp signalbot.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now signalbot
   ```
3. Read the logs (see [Logs](TECHNICAL.md#logs)):
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

Docker's journald driver records stderr at priority 3 (err) and stdout at 6 (info), so warnings and errors, which the bot writes to stderr, both show as errors there (see [Logs](TECHNICAL.md#logs)).

### Then

Add the bot's account to a group and @mention it, reply to one of its messages, or DM it from an allowed sender. Send it `/intro` or `/help` to see its introduction (`INTRO`), the same text the model gives when asked to introduce itself; that reply never goes to the model.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `SIGNALCLI_SOCKET` | required | Unix socket path, or `host:port` for TCP |
| `SIGNAL_DM_USER_IDS` | empty | UUIDs/numbers allowed to DM the bot, separated by commas or whitespace |
| `MODEL` | `deepseek/deepseek-v4.1-flash-1017` | The model's name at the API: on OpenRouter as `provider/model` |
| `MODEL_API_KEY` | empty | API key for the model's API, such as your [OpenRouter](https://openrouter.ai) key; none is sent when empty. With neither this nor `MODEL_API_URL` the bot has [no model](docs/NO_MODEL.md) |
| `MODEL_API_URL` | `https://openrouter.ai/api/v1` | Base URL of an OpenAI-compatible chat completions API; see [Other providers](docs/PROVIDERS.md) |
| `MODEL_REASONING` | `low` | Reasoning effort, e.g. `low`, `medium`, `high` (`none` to omit). Sent as OpenRouter's `reasoning.effort`, or elsewhere as `reasoning_effort` |
| `MODEL_SEARCH` | `true` | Lets the model search the web (OpenRouter's web search tool) when it decides to, citing the sites it uses by url; searches are billed. Only on OpenRouter: on another API it is left out, with a warning. Accepts `true`/`false`, `yes`/`no`, `on`/`off`, `1`/`0` |
| `MODEL_SYSTEM_PROMPT` | empty | Extra instructions, appended to the built-in prompt (which keeps answers short and Signal-sized and, when the model has tools, asks it to name its source) |
| `DAILY_BUDGET` | `1` | US dollars each group and each DM may spend on the model a day, midnight to midnight in `TZ`; `0` for no cap. Once a chat reaches it, the bot says once "I've reached today's spending limit for this chat; I'll be back in 5 hours." and answers nothing else there until midnight, except commands that don't use the model. Today's spending is saved in `budget.json` in `STATE_DIRECTORY`, so a restart doesn't reset it. Only OpenRouter reports costs, so with another API it isn't applied |
| `DARKMODE` | `off` | Chart colours: `off` for light charts, `on` for dark, or the hours charts are dark in `TZ`, such as `18:00-6:00`, `18-6` or `6pm-6am` |
| `GROUP_CONTEXT` | `true` | Remember group messages not addressed to the bot as context, so "what do you think @bot" works without replying to the message; they are never logged |
| `HISTORY_MAX` | `10` | Most messages remembered per chat, the bot's replies and, in groups, messages not addressed to it included. When full, the older half is dropped, so the start of the model's request stays the same for several messages and comes from the API's prompt cache (cheaper and faster); below 3, the oldest message is dropped |
| `INTRO` | built-in sentence | Overrides the self-introduction; `{name}`, `{model}` (without any OpenRouter `provider/` prefix) and `{tools}` (the names of the tools, or `none`) are substituted |
| `MAX_CONCURRENCY` | `32` | Conversations in flight at once; messages beyond this are dropped |
| `MCP_CONFIG` | empty | Path of an MCP servers file; see [MCP tools](docs/MCP.md) |
| `STATE_DIRECTORY` | working directory | Where features save their state, one file each, such as `/schedule`'s `schedules.json` (and the core its `budget.json`); the bot won't start if a feature that saves can't list and write it. Set by systemd's `StateDirectory=signalbot` (`/var/lib/private/signalbot`) and by the Docker image (`/data`, which must be writable by user 65532) |
| `TOOL_LINGER_ROUNDS` | `2` | Replies a keyword-gated tool group (charts, an MCP server with `keywords`) stays offered after the last one that matched its keywords or used its tools; `0` offers it only on a matching message. Any use or match starts the count again, so a run of follow-ups keeps it. Capped at `HISTORY_MAX`. See [How tools are offered](docs/TOOLS.md) |
| `TZ` | `UTC` in Docker, the host's under systemd | Time zone for `/schedule` times without an offset and for `DARKMODE` hours, e.g. `Australia/Sydney` |

`OPENROUTER_API_KEY` is now `MODEL_API_KEY`, and `REASONING` and `SYSTEM_PROMPT` are now `MODEL_REASONING` and `MODEL_SYSTEM_PROMPT`; the old names are ignored. `LOG_LEVEL` is gone: every level is always written (see [Logs](TECHNICAL.md#logs)).

The default `INTRO` is short paragraphs: `hi, I'm *{name}*, your open source assistant running {model}.`, `I respond to @{name} and replies to my messages.`, `Tools:` then `{tools}`, one tool category per paragraph, then `{more}`, lines features add (such as `Type "/schedule" for schedules and reminders.`, only in a DM, where schedules are made), then the link `https://github.com/robdevops/signalbot/blob/main/README.md` (Signal makes a bare url clickable; it has no named links).

The cost logged with each reply is the amount OpenRouter reports billing for it. An API that reports no cost gets none logged.

## Guides

- [How tools are offered](docs/TOOLS.md)
- [Files](docs/FILES.md)
- [Links](docs/LINKS.md)
- [Other providers](docs/PROVIDERS.md)
- [No model](docs/NO_MODEL.md)
- [MCP tools](docs/MCP.md)
- [Scheduling](docs/SCHEDULING.md)
- [Troubleshooting](docs/TROUBLESHOOTING.md)
