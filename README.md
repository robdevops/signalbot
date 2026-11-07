# signalbot

A small, [privacy-conscious](PRIVACY.md) chat bot that connects to a running [signal-cli](https://github.com/AsamK/signal-cli) daemon and answers using a model from [OpenRouter](https://openrouter.ai) or a [compatible provider](docs/MODELS.md).

- [Features](#features)
- [Use cases](#use-cases)
- [Install](#install)
  - [Prerequisites](#prerequisites)
  - [Configuration](#configuration)
  - [Install as a systemd service](#install-as-a-systemd-service)
  - [Install as a Docker container](#install-as-a-docker-container)
  - [Test](#test)
- [Docs](#docs)

## Features

- Responds to @mentions, replies to its messages, and DMs from whitelisted contacts
- Tools: web search, line and pie charts, and your own [MCP connectors](docs/FILES_LINKS_TOOLS_MCP.md#mcp-connectors)
- [`/schedule`](docs/SCHEDULING.md) a message or command, once or repeating
- Understands [files](docs/FILES_LINKS_TOOLS_MCP.md#files) (PDFs, text and code, Word, Excel and PowerPoint) and the [pages you link to](docs/FILES_LINKS_TOOLS_MCP.md#links), and if the model supports it, images, stickers, voice notes and other audio
- Typing indicators, reactions, stickers, @mentions and replies
- In-memory conversation context (Default: 10 messages). Never logged to disk, forgotten after 24 hours, or sooner if _disappearing messages_ requires.

## Use cases

- Your group's brain trust: ask it to explain things, check facts or settle debates, or just the fun of a sassy LLM in your Signal groups
- Explain or translate any message, photo, document, or website by replying to it and @mentioning the bot
- Connect MCP tools, data, and charting to your group chat
- A private assistant in your DMs
- Use this project as the basis of your own bot project

The messages the bot answers, and the names in them, are sent to the model's API (OpenRouter, unless you [choose another](docs/MODELS.md)). See [PRIVACY.md](PRIVACY.md) for what happens to your messages.

## Install

### Prerequisites

Signalbot requires a running [signal-cli](https://github.com/AsamK/signal-cli) daemon with your Signal account linked, listening on `127.0.0.1:7583` (`signal-cli -o json daemon --tcp 127.0.0.1:7583 --no-receive-stdout`).

[`examples/signal-cli.service`](examples/signal-cli.service) is an example systemd unit that runs signal-cli as above. Edit `User=` and the path in `ExecStart`, then copy it to `/etc/systemd/system/`, and run `sudo systemctl daemon-reload` and `sudo systemctl enable --now signal-cli`.

Secure your signal account details:

```
chmod 700 ~/.local/share/signal-cli
```

> [!WARNING]
> **On TCP, anyone with a login or a process on the host can connect to `127.0.0.1:7583` and read or send messages as your bot's Signal account.** To harden your security, use signal-cli's [Unix socket](docs/SOCKET.md) instead.

### Configuration

Get the code and create the config, which is root-only since it holds your API key:

```
git clone https://github.com/robdevops/signalbot.git
cd signalbot
sudo install -m 660 .env.example /etc/signalbot.env
sudoedit /etc/signalbot.env
```

To get the bot working with Signal and a model on OpenRouter, set these:

| Variable | Default | Description |
|---|---|---|
| `SIGNALCLI_SOCKET` | `127.0.0.1:7583` | signal-cli's `host:port`, or its [Unix socket](docs/SOCKET.md) path |
| `MODEL_API_KEY` | empty | Your [OpenRouter](https://openrouter.ai) API key (or see [other providers](docs/MODELS.md)) |
| `MODEL` | `deepseek/deepseek-v4.1-flash-1017` | The model, as OpenRouter's `provider/model` |
| `SIGNAL_DM_USER_IDS` | empty | UUIDs or numbers allowed to DM the bot, separated by commas or whitespace; empty turns DMs off |

Every other setting (daily budget, chart colours, history length, MCP connectors, time zone, …) is in [Configuration](docs/CONFIGURATION.md). Then install the bot with systemd or with Docker.

### Install as a systemd service

The bot runs as a dynamic user (`DynamicUser=yes`). Its schedules and budget are kept in `/var/lib/private/signalbot`, which systemd creates.

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
3. Read the logs:
   ```
   journalctl -fu signalbot
   journalctl -u signalbot -p warning   # warnings and errors only
   ```

### Install as a Docker container

`--network=host` lets the container reach signal-cli on the host's `127.0.0.1`. Run it with `sudo`, because Docker reads `--env-file` as the calling user and the file is root-only.

Schedules are saved in `/data` in the container (`STATE_DIRECTORY=/data`), bind-mounted from the same host directory systemd would use. The container runs as user 65532, which must own it, or the bot exits asking for this:

```
sudo mkdir -p /var/lib/private/signalbot
sudo chown 65532 /var/lib/private/signalbot
```

Use one install per host, systemd or Docker: the unit's dynamic user and the container's user 65532 can't share the directory.

```
sudo docker build --build-arg VERSION=$(git describe --tags --always --dirty) -t signalbot .
sudo docker run -d --name signalbot --restart=always --env-file /etc/signalbot.env \
  --network=host -v /var/lib/private/signalbot:/data \
  $(test -S /run/systemd/journal/socket && echo --log-driver=journald || echo --log-driver=local) \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges --pids-limit=64 --memory=300m --memory-swap=300m --ulimit core=0 \
  signalbot
```

Read the logs:

```
docker logs -f signalbot
journalctl CONTAINER_NAME=signalbot -p err   # warnings and errors only, where Docker logs to journald
```

### Test

Add the bot's account to a group and @mention it, reply to one of its messages, or DM it from an allowed sender. Send it `/intro` or `/help` to see its introduction.

## Docs

- [Configuration](docs/CONFIGURATION.md)
- [Unix socket](docs/SOCKET.md)
- [Files, Links, Tools, and MCP connectors](docs/FILES_LINKS_TOOLS_MCP.md)
- [Language models](docs/MODELS.md)
- [Scheduling](docs/SCHEDULING.md)
- [Troubleshooting](docs/TROUBLESHOOTING.md)
