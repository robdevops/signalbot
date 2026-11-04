# signalbot

A small, [privacy-conscious](PRIVACY.md) chat bot that connects to a running [signal-cli](https://github.com/AsamK/signal-cli) daemon over its socket and answers using a model from [OpenRouter](https://openrouter.ai) or [another provider](#other-providers).

- [Features](#features)
- [Use cases](#use-cases)
- [Prerequisites](#prerequisites)
- [Install](#install)
  - [With systemd](#with-systemd)
  - [With Docker](#with-docker)
  - [Then](#then)
- [Configuration](#configuration)
  - [Other providers](#other-providers)
  - [No model](#no-model)
  - [MCP tools](#mcp-tools)
  - [Scheduling](#scheduling)
- [Troubleshooting](#troubleshooting)

## Features

- Responds to @mentions, replies to its messages, and DMs from senders in `SIGNAL_DM_USER_IDS`
- Tools: web search, built-in line and pie charts, and your own [MCP](#mcp-tools) tools
- [`/schedule`](#scheduling) a message or command, once or repeating, in a DM or a group
- Understands images and stickers (if the configured model supports vision), [files](#files) (PDFs, voice notes and other audio if the model supports audio, text and code, Word, Excel and PowerPoint), and the [pages you link to](#links)
- Supports various Signal features: typing indicators, reactions, stickers, contacts, @mentions, quoted replies
- Short per-conversation memory, for context. Keeps at most 10 messages (HISTORY_MAX), dropping the older half when full, and frees them after 24 hours, or when Signal's _disappearing messages_ timer expires, whichever comes first. Names and message bodies are never logged to disk.

## Use cases

- Your group's brain trust: ask it to explain things, check facts or settle debates
- Explain or translate any message or photo by replying to it and tagging the bot
- Connect MCP tools, data, and charting to your group chat
- Modify the bot to add your own /commands (see `/intro`)
- A private assistant in your DMs, or just the fun of a sassy LLM in your Signal groups

The messages the bot answers, and the names in them, are sent to the model's API (OpenRouter, unless you [choose another](#other-providers)). See [TECHNICAL.md](TECHNICAL.md) and [PRIVACY.md](PRIVACY.md) for details on how it works.

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

Create the config. The file holds your API key, so it is root-only; edit it, setting `MODEL_API_KEY` (your [OpenRouter](https://openrouter.ai) key; see [Other providers](#other-providers) and [No model](#no-model) for the alternatives) and `SIGNAL_DM_USER_IDS`:

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
| `MODEL_API_KEY` | empty | API key for the model's API, such as your [OpenRouter](https://openrouter.ai) key; none is sent when empty. With neither this nor `MODEL_API_URL` the bot has [no model](#no-model) |
| `MODEL_API_URL` | `https://openrouter.ai/api/v1` | Base URL of an OpenAI-compatible chat completions API; see [Other providers](#other-providers) |
| `MODEL_REASONING` | `low` | Reasoning effort, e.g. `low`, `medium`, `high` (`none` to omit). Sent as OpenRouter's `reasoning.effort`, or elsewhere as `reasoning_effort` |
| `MODEL_SEARCH` | `true` | Lets the model search the web (OpenRouter's web search tool) when it decides to, citing the sites it uses by url; searches are billed. Only on OpenRouter: on another API it is left out, with a warning. Accepts `true`/`false`, `yes`/`no`, `on`/`off`, `1`/`0` |
| `MODEL_SYSTEM_PROMPT` | empty | Extra instructions, appended to the built-in prompt (which keeps answers short and Signal-sized and, when the model has tools, asks it to name its source) |
| `DAILY_BUDGET` | `1` | US dollars each group and each DM may spend on the model a day, midnight to midnight in `TZ`; `0` for no cap. Once a chat reaches it, the bot says once "I've reached today's spending limit for this chat; I'll be back in 5 hours." and answers nothing else there until midnight, except commands that don't use the model. Today's spending is saved in `budget.json` in `STATE_DIRECTORY`, so a restart doesn't reset it. Only OpenRouter reports costs, so with another API it isn't applied |
| `DARKMODE` | `off` | Chart colours: `off` for light charts, `on` for dark, `auto` for dark from 21:00 to 07:00 in `TZ` and light otherwise |
| `GROUP_CONTEXT` | `true` | Remember group messages not addressed to the bot as context, so "what do you think @bot" works without replying to the message; they are never logged |
| `HISTORY_MAX` | `10` | Most messages remembered per chat, the bot's replies and, in groups, messages not addressed to it included. When full, the older half is dropped, so the start of the model's request stays the same for several messages and comes from the API's prompt cache (cheaper and faster); below 3, the oldest message is dropped |
| `INTRO` | built-in sentence | Overrides the self-introduction; `{name}`, `{model}` (without any OpenRouter `provider/` prefix) and `{tools}` (the names of the tools, or `none`) are substituted |
| `MAX_CONCURRENCY` | `32` | Conversations in flight at once; messages beyond this are dropped |
| `MCP_CONFIG` | empty | Path of an MCP servers file; see [MCP tools](#mcp-tools) |
| `STATE_DIRECTORY` | working directory | Where `/schedule` saves `schedules.json`; the bot won't start if it can't list and write it. Set by systemd's `StateDirectory=signalbot` (`/var/lib/private/signalbot`) and by the Docker image (`/data`, which must be writable by user 65532) |
| `TOOL_LINGER_ROUNDS` | `2` | Replies a keyword-gated tool group (charts, an MCP server with `keywords`) stays offered after the last one that matched its keywords or used its tools; `0` offers it only on a matching message. Any use or match starts the count again, so a run of follow-ups keeps it. Capped at `HISTORY_MAX`. See [How tools are offered](#how-tools-are-offered) |
| `TZ` | `UTC` in Docker, the host's under systemd | Time zone for `/schedule` times without an offset and for `DARKMODE=auto`, e.g. `Australia/Sydney` |

`OPENROUTER_API_KEY` is now `MODEL_API_KEY`, and `REASONING` and `SYSTEM_PROMPT` are now `MODEL_REASONING` and `MODEL_SYSTEM_PROMPT`; the old names are ignored. `LOG_LEVEL` is gone: every level is always written (see [Logs](TECHNICAL.md#logs)).

The default `INTRO` is short paragraphs: `hi, I'm *{name}*, your open source assistant running {model}.`, `I respond to @{name} and replies to my messages.`, `Tools:` then `{tools}`, one tool category per paragraph, then `{more}`, lines features add (such as `Type "/schedule" for schedules and reminders.`, only in a DM, where schedules are made), then the link `https://github.com/robdevops/signalbot/blob/main/README.md` (Signal makes a bare url clickable; it has no named links).

The cost logged with each reply is the amount OpenRouter reports billing for it. An API that reports no cost gets none logged.

### How tools are offered

Web search and tools without `keywords` are offered with every message. Charts, and MCP servers with `keywords`, are offered only when a message matches their words, then for `TOOL_LINGER_ROUNDS` more replies, and each reply that matches or uses one of their tools starts that count again. So "chart NVDA", "add AMD", "make it a year" keeps the charts on however long it runs; only replies that leave them unused count down.

The default of 2 is meant to cover a short aside ("thanks!", "what's AMD's ticker again?") before you come back to the chart, without the tools still being on when the conversation has moved on. Lingering longer has two costs: a tool on for unrelated messages is one the model may use when it shouldn't (a stock tool picked for a population question), and its descriptions add to every request. The prompt cache gains little, because the offered tools change once either way when a group drops off. Raise it if people often come back to a chart after several other questions; set `0` to offer a group only on messages that name it.

### Files

Send the bot a file in a DM, attach one when you @mention it or reply to it, or @mention it in reply to someone else's file. Each reaches the model in the form it can use:

| File | What the model gets |
|---|---|
| PDF | The PDF itself. A model that reads files natively gets it as it is; for any other, OpenRouter reads it first with its `mistral-ocr` engine, which handles tables, columns and scanned pages |
| Audio, including voice notes | The audio, if the model takes audio (OpenRouter's model list says); otherwise a note that it can't hear it |
| Text and code (`.txt`, `.md`, `.csv`, `.json`, source files, …) | Its text, up to 50,000 characters |
| Word, Excel, PowerPoint (`.docx`, `.xlsx`, `.pptx`) | Its text: paragraphs, cells by sheet, slides in order |
| Anything else | A note that it was attached and can't be read |

Up to 4 files a message, 12 MB each and 16 MB together (the model is told of any left out). **OCR costs money:** OpenRouter charges about $2 per 1,000 pages, so a 150-page filing is about $0.30 each time it's sent, on top of the reply, and shows in the reply's logged cost. The bot never keeps a file, so asking about it again means replying to it again, and it's read again. To fetch the original of a file someone else posted, the bot remembers recent attachments' IDs and names in memory for a day; see [PRIVACY.md](PRIVACY.md).

### Links

A message that addresses the bot and has a URL, or replies to a message with one, gets the page read for the model: up to 2 links, the page's main text up to 15,000 characters, a PDF link as a PDF (see [Files](#files)). Only links a person sent are fetched, never ones the model or a page suggests, so a link in a reply to one of the bot's own messages isn't fetched. A link to a private or local address (the bot's own machine, your network, a cloud metadata service) is refused, however its name resolves, and the model is told it couldn't load it.

### Other providers

The bot speaks OpenAI's chat completions format, which most providers and local servers offer: OpenAI, Ollama, LM Studio, vLLM, llama.cpp, Groq, Together, Mistral, DeepSeek, and the compatible endpoints of Gemini and Anthropic. Set `MODEL_API_URL` to the API's base URL (the part before `/chat/completions`), `MODEL` to the model's name there, and `MODEL_API_KEY` if it needs one. For example, a local [Ollama](https://ollama.com), which needs no key:

```
MODEL_API_URL=http://localhost:11434/v1
MODEL=qwen3
MODEL_REASONING=none
```

What differs from OpenRouter:

- OpenRouter's extras are sent only to OpenRouter: the request not to use providers that collect data, and the request for the cost. Read your provider's own data policy.
- `MODEL_REASONING` goes as the standard `reasoning_effort`. Some APIs reject it for a model that doesn't reason; set `none` to leave it out.
- Only OpenRouter's model list says what a model supports, so elsewhere the bot assumes the model takes images and can call tools, as the startup log says. If the API rejects a request with images (a 400 or 422), the bot logs a warning, answers that message without them (the model is told it can't see them) and sends no more images until it restarts; the same goes for tools, and chat commands then answer that they need tools. Usually only that first rejected request is wasted; when a message carried both and the tools were the problem, two are, and images stay on.
- OpenRouter's web search runs only on OpenRouter; elsewhere it is left out, with a warning at startup.
- An `https://` API must offer TLS 1.3. A local one can be plain `http://`.
- Under Docker, `localhost` is the container itself: reach a server on the host with `--add-host=host.docker.internal:host-gateway` and `MODEL_API_URL=http://host.docker.internal:11434/v1`, with the server listening on the Docker bridge address as well as 127.0.0.1 (for Ollama, `OLLAMA_HOST=172.17.0.1:11434`, which the host and its containers can both reach), or with its port firewalled. Ollama has no authentication, so `OLLAMA_HOST=0.0.0.0` lets anyone who can reach the port use it (PRIVACY RISK).

### No model

With neither `MODEL_API_KEY` nor `MODEL_API_URL` set, the bot runs without a model, and its startup log says "no model: only commands are answered". Commands still work: `/intro`, `/schedule` and its aliases, and scheduled plain text is posted as written; `/intro` then says it runs no model. Anything else addressed to it, a mention, a reply or a DM, gets no reply, no typing indicator and no model request. Nothing is kept in history, not even group context, and MCP servers aren't started.

### MCP tools

The model can call tools from [MCP](https://modelcontextprotocol.io) servers, remote (`url`) or local (`command`). Anyone who can address the bot can make it call them, so choose what each server offers: `allowed_tools` offers only the tools listed, `blocked_tools` offers all but those, and without either every tool the server has is offered. Put the servers in a file, for example `/etc/signalbot-mcp.json`:

```json
{"mcpServers": {
  "weather": {
    "url": "https://example.com/mcp",
    "headers": {"Authorization": "Bearer …"},
    "allowed_tools": ["get_forecast", "get_alerts"],
    "instructions": "Use the weather tools for any forecast, and give temperatures in °C.",
    "keywords": ["weather", "forecast", "rain", "temperature"],
    "chat_commands": {
      "forecast": {"tool": "get_forecast", "require_arg": true},
      "alerts": {"tool": "get_alerts"}
    }
  },
  "files": {
    "command": "uvx",
    "args": ["some-mcp-server"],
    "env": {"KEY": "value"},
    "blocked_tools": ["delete_file"],
    "disabled": true
  }
}}
```

| Key | What it does |
|---|---|
| `url`, `headers` | a remote server (Streamable HTTP or SSE), with optional HTTP headers |
| `command`, `args`, `env` | a local server, run with only `PATH`, `HOME` and its `env` |
| `allowed_tools` / `blocked_tools` | offer only these tools, or every tool but these |
| `instructions` | a line added to the system prompt while the server's tools are offered (see below) |
| `keywords` | offer the server's tools only when a message has one of these words (see below) |
| `chat_commands` | optional slash-command shortcuts to a tool (you can always just ask): `tool` names it, `require_arg` makes a bare command get a usage reply |
| `disabled` | leave the server out without deleting it |

**You don't need a command to use the tools: just ask.** For example, `@signalbot what's the weather in Sydney this weekend?` The model sees every tool that's offered and decides when to call one. Without `keywords` a server's tools are always offered; with them, the message needs one of its words, such as `weather` or `forecast` above. Gating tools this way saves tokens.

Two optional fields shape how a server's tools are offered:

- `instructions` is a line added to the system prompt while the server's tools are offered. MCP servers can send their own `instructions` when the bot connects (the MCP spec's hint for the system prompt); without this key those are used, capped at 1,000 characters. Set it to replace them, or to `""` to use none.
- `keywords` offers the server's tools only when a message contains one of the words, ignoring capitals and matching whole words only, and for the `TOOL_LINGER_ROUNDS` replies (default 2) after one that matched or used its tools. Without it, the tools are always offered. A chat command always gets its tool.

`chat_commands` are optional shortcuts on top of asking. `/forecast sydney this weekend` asks the model, which gets that tool on top of whatever else is on and must call it first. With `require_arg`, a bare `/forecast` gets a usage reply from the bot without asking the model.

A server that can't be reached at startup is logged and left out. A tool named in either list that the server doesn't have is a config error, so a typo can't leave a blocked tool offered. Local servers get only `PATH`, `HOME` and their `env`, never the bot's own environment.

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

For example `/schedule "@signalbot what's the weather today?" 2030-01-31T07:30 every day Family`. At each run the text acts as if you had just posted it, in the DM or the named group: plain text is posted by the bot as written (with @Name turned into real mentions, and Markdown such as `**bold**` shown as bold), text that @mentions the bot gets its answer, and a `/command` runs.

- The time is like `2030-01-31T09:00`, in the bot's time zone (`TZ`), or with an offset, like `2030-01-31T09:00+10:00`. A one-off in the past is refused; a repeating schedule that starts in the past first runs at its next time. A monthly schedule from the 31st runs on the last day of shorter months.
- The group name is the rest of the line, with no quotes needed. It is matched among the groups you share with the bot, ignoring capitals, emoji and extra spaces, so `family` finds "🏡 Family 🎉". No match, or more than one, is refused with your groups listed. A schedule keeps the group's ID, so it survives a rename; if you leave the group its runs are dropped until you delete it.
- `/timer`, `/reminder` and `/job` work the same as `/schedule`.
- When the model can call tools, it is told these four commands, so asking it for a reminder, or mistyping one as `/time`, gets you pointed to them.
- Straight and curly quotes both work. A schedule can't run `/schedule`, `/timer`, `/reminder` or `/job`, so it can't make more schedules. The bot never answers its own posts, so a scheduled message that @mentions it gets exactly one answer.
- If the bot was down when a run was due, a repeating schedule skips it and a one-off up to an hour late still runs; an older one-off is dropped.
- Schedules are saved in `schedules.json` in `STATE_DIRECTORY` until deleted. See [PRIVACY.md](PRIVACY.md).

## Troubleshooting

**The bot answers only commands, and ignores mentions and DMs.** It has no model: the startup log says "no model: only commands are answered" (a notice, which `journalctl -p warning` hides). Set `MODEL_API_KEY`, or `MODEL_API_URL` for a provider that needs no key. `OPENROUTER_API_KEY` is now `MODEL_API_KEY`, and the old name is ignored.

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
