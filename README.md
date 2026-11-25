# signalbot

A small, [privacy-conscious](PRIVACY.md) chat bot for Signal, through a running [signal-cli](https://github.com/AsamK/signal-cli) daemon, or for [Telegram](#telegram), that answers using a model from [OpenRouter](https://openrouter.ai) or a [compatible provider](docs/MODELS.md).

- [Features](#features)
- [Use cases](#use-cases)
- [Install](#install)
  - [Prerequisites](#prerequisites)
  - [Configuration](#configuration)
  - [Install as a systemd service](#install-as-a-systemd-service)
  - [Install as a Docker container](#install-as-a-docker-container)
  - [Telegram](#telegram)
  - [Test](#test)
- [Docs](#docs)

## Features

- LLM responds to @mentions, replies to its messages, and DMs from whitelisted contacts
- Tools: web search, line charts (by day, or by the hour within a day) and pie charts, [pictures](docs/FILES_LINKS_TOOLS_MCP.md#images) (drawings, art, diagrams) with GPT Image 2, and your own [MCP connectors](docs/FILES_LINKS_TOOLS_MCP.md#mcp-connectors)
- [Reminders](docs/SCHEDULING.md), and [alerts](docs/FEEDS.md) from your MCP servers
- Game plugins: [geoguesser](https://github.com/robdevops/GeoGuess) | [song guesser](https://github.com/robdevops/SongGuess) | [trivia](https://github.com/robdevops/TriviaQuiz), or [write your own](docs/FILES_LINKS_TOOLS_MCP.md#addons)
- Understands [files](docs/FILES_LINKS_TOOLS_MCP.md#files) (PDFs, text and code, Word, Excel and PowerPoint) and the [pages you link to](docs/FILES_LINKS_TOOLS_MCP.md#links), and if the model supports it, images, stickers, voice notes and other audio
- Typing indicators, reactions, stickers, @mentions and replies
- In-memory conversation context (Default: 10 messages). Never logged to disk, forgotten after 24 hours, or sooner if _disappearing messages_ requires.

## Use cases

- Your group's brain trust: ask it to explain things, check facts or settle debates, or just the fun of a sassy LLM in your Signal groups
- Explain or translate any message, photo, document, or website by replying to it and @mentioning the bot
- Connect MCP tools, data, and charting to your group chat
- Play games in your group: [GeoGuess](https://github.com/robdevops/GeoGuess), guessing where a street photo was taken; [TriviaQuiz](https://github.com/robdevops/TriviaQuiz), a trivia quiz the model writes on any topic; and [SongGuess](https://github.com/robdevops/SongGuess), a song quiz of short clips from Deezer
- A private assistant in your DMs
- Use this project as the basis of your own bot project

The messages the bot answers, and the names in them, are sent to the model's API (OpenRouter, unless you [choose another](docs/MODELS.md)). See [PRIVACY.md](PRIVACY.md) for what happens to your messages.

## Install

### Prerequisites

Signalbot requires a running [signal-cli](https://github.com/AsamK/signal-cli) daemon with your Signal account linked, listening on `127.0.0.1:7583` (`signal-cli -o json daemon --tcp 127.0.0.1:7583 --no-receive-stdout`).

[`examples/signal-cli.service`](examples/signal-cli.service) runs signal-cli as above. Edit `User=` and the path in `ExecStart`, then copy it to `/etc/systemd/system/`, and run `sudo systemctl daemon-reload` and `sudo systemctl enable --now signal-cli`.

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

[`compose.yaml`](compose.yaml) runs the bot with Docker Compose, hardened (read-only, no capabilities, memory and process limits) and on the host's network, so it reaches signal-cli on `127.0.0.1`. Run it with `sudo`, because Compose reads `/etc/signalbot.env` as the calling user and the file is root-only.

Schedules are saved in `/data` in the container, bind-mounted from the same host directory systemd would use. The container runs as user 65532, which must own it, or the bot exits asking for this:

```
sudo mkdir -p /var/lib/private/signalbot
sudo chown 65532 /var/lib/private/signalbot
```

Use one install per host, systemd or Docker: the unit's dynamic user and the container's user 65532 can't share the directory.

Build and start it, from the code's directory:

```
sudo VERSION=$(git describe --tags --always --dirty) LOG_DRIVER=$(test -S /run/systemd/journal/socket && echo journald || echo local) docker compose up -d --build
```

Read the logs:

```
docker logs -f signalbot
```

Logs go to journald where the host has it, otherwise to Docker's rotating `local` driver; `docker logs` reads both. Under journald, `journalctl CONTAINER_NAME=signalbot -p err` shows warnings and errors.

### Build without charts, Signal or Telegram

Charts and their fonts can be left out of the binary (about 0.7 MB of 10 MB) with a build tag: `go build -tags nocharts`, or for Docker, add `TAGS=nocharts` after `sudo` in the build command above. Likewise `nosignal` or `notelegram` leaves out a chat network, and tags combine with commas (`-tags nocharts,notelegram`); leaving out both networks fails to build. To stop files reaching the model without rebuilding, set `FILES=off` instead (see [Configuration](docs/CONFIGURATION.md)).

### Block someone, or other signal-cli chores

While the bot runs, signal-cli's daemon owns the account, so `signal-cli` itself can't be run alongside it. `signalctl` sends one request over the same socket the bot uses (`SIGNALCLI_SOCKET`):

```
go run ./cmd/signalctl block +15551234567 group:Z3JvdXA=
go run ./cmd/signalctl unblock +15551234567
go run ./cmd/signalctl blocked
go run ./cmd/signalctl call listGroups '{"detailed":true}'
```

With Docker, it is in the image: `docker exec signalbot /signalctl blocked`. Under systemd, run it as a user that can reach the socket (anyone on TCP; the `signal-cli` group for the [Unix socket](docs/SOCKET.md)), for example `sudo -u signal-cli SIGNALCLI_SOCKET=/run/signal-cli/socket ./signalctl blocked`.

A block is Signal's own, as on a phone: signal-cli drops everything from that person or group from then on, including their deletes of messages the bot already holds, and the block syncs to linked devices. `call` sends any method in signal-cli's [JSON-RPC docs](https://github.com/AsamK/signal-cli/blob/master/man/signal-cli-jsonrpc.5.adoc). The daemon also sends signalctl's connection the messages it receives; signalctl skips them unread, so it never prints anyone's text.

### Telegram

The same bot can answer on Telegram instead, talking to Telegram's Bot API directly (long polling, so it needs no public address). A bot answers on one network: to run on both, run two, each with its own config and state directory.

1. In Telegram, message [@BotFather](https://t.me/BotFather), send `/newbot`, and copy the token it gives you.
2. In `/etc/signalbot.env`, set `TELEGRAM_BOT_TOKEN` to the token, and `TELEGRAM_DM_USER_IDS` to the user IDs allowed to DM the bot (a user can find theirs by messaging a bot such as [@userinfobot](https://t.me/userinfobot)). Leave `SIGNALCLI_SOCKET` unset. The token is a password: anyone with it controls the bot.
3. Install as above, skipping signal-cli and its socket.
4. Add the bot to a group and @mention it, or reply to one of its messages.

Anyone on Telegram can find a bot by its username and add it to a group of their own, so the bot answers in a group only while someone in `TELEGRAM_DM_USER_IDS` is a member, and leaves a group where no one is. To stop it being added at all, send BotFather `/setjoingroups` and choose Disable once it is in your groups. In a group with topics, each topic is its own conversation, with its own history, and the bot answers in the topic it was asked in.

**Privacy mode.** By default Telegram sends a bot in a group only the messages addressed to it, so the bot has no group context. To give it the conversation, send BotFather `/setprivacy` and choose Disable, then remove the bot from the group and add it again; or make it a group admin. As admin it also sees reactions and members joining and leaving.

Telegram differs from Signal in a few ways: bot chats are not end-to-end encrypted, the Bot API never tells a bot about deleted messages, and a reply is capped at 4,096 characters a message. See [PRIVACY.md](PRIVACY.md#telegram).

### Test

Add the bot's account to a group and @mention it, reply to one of its messages, or DM it from an allowed sender. Send it `/intro` or `/help` to see its introduction.

## Docs

- [Configuration](docs/CONFIGURATION.md)
- [Unix socket](docs/SOCKET.md)
- [Files, Links, Images, Tools, and MCP connectors](docs/FILES_LINKS_TOOLS_MCP.md)
- [Language models](docs/MODELS.md)
- [Scheduling](docs/SCHEDULING.md)
- [Feeds](docs/FEEDS.md)
- [Troubleshooting](docs/TROUBLESHOOTING.md)
