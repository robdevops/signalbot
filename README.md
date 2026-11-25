# signalbot

A small, [privacy-conscious](PRIVACY.md) chat bot for Signal, through a running [signal-cli](https://github.com/AsamK/signal-cli) daemon, or for [Telegram](#telegram), [Discord](#discord) or [Slack](#slack), that answers using a model from [OpenRouter](https://openrouter.ai) or a [compatible provider](docs/MODELS.md).

- [Features](#features)
- [Use cases](#use-cases)
- [Install](#install)
  - [Prerequisites](#prerequisites)
  - [Configuration](#configuration)
  - [Install as a systemd service](#install-as-a-systemd-service)
  - [Install as a Docker container](#install-as-a-docker-container)
  - [Telegram](#telegram)
  - [Discord](#discord)
  - [Slack](#slack)
  - [Test](#test)
- [Docs](#docs)

## Features

- LLM responds to @mentions, replies to its messages, and DMs (on Signal from group admins, or the contacts you list)
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
| `SIGNAL_DM_USERS` | `admins, mods` | Who may DM the bot: unset, an admin of a group the bot is in (Signal groups have no mods); `all`, anyone; `none`, no one; or UUIDs or numbers, with or without `signal:`, alone or with `admins`, separated by commas or whitespace. See [Who may use the bot](docs/CONFIGURATION.md#who-may-use-the-bot) |
| `SIGNAL_GROUP_IDS` | `all` | The groups the bot answers in, by ID, separated by commas; `none` for none. Unset (`all`), it answers in any group anyone adds it to. Recommended: set it, see below |

**Set `SIGNAL_GROUP_IDS`.** Unset, anyone who can add the bot to a group gets it to answer there, at your cost. Once the bot is in your groups, start it: it warns that it answers in any group and logs the line to set, with each group's name after `#` so you can check them (`journalctl -u signalbot | grep SIGNAL_GROUP_IDS=`), such as `SIGNAL_GROUP_IDS=dGVzdC1...=,b3RoZXI...=  # Family, Book club`. Copy it into `/etc/signalbot.env` (anything after `#` is ignored) and restart. To add a group later, add the bot to it, then add its ID, which `go run ./cmd/signalctl call listGroups` lists (see [below](#block-someone-or-other-signal-cli-chores)).

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

### Build without charts, Signal, Telegram, Discord or Slack

Charts and their fonts can be left out of the binary (about 0.7 MB of 10 MB) with a build tag: `go build -tags nocharts`, or for Docker, add `TAGS=nocharts` after `sudo` in the build command above. Likewise `nosignal`, `notelegram`, `nodiscord` or `noslack` leaves out a chat network, and tags combine with commas (`-tags nocharts,notelegram`); leaving out every network fails to build. To stop files reaching the model without rebuilding, set `FILES=off` instead (see [Configuration](docs/CONFIGURATION.md)).

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

The same bot can answer on Telegram instead, or as well, talking to Telegram's Bot API directly (long polling, so it needs no public address).

1. In Telegram, message [@BotFather](https://t.me/BotFather), send `/newbot`, and copy the token it gives you.
2. In `/etc/signalbot.env`, set `TELEGRAM_BOT_TOKEN` to the token. Leave `SIGNALCLI_SOCKET` unset for Telegram alone. The token is a password: anyone with it controls the bot. `TELEGRAM_DM_USERS` says who may DM it: unset, only the owner or an administrator of a group the bot is in with them; or `all`, `none`, or user IDs (with or without `telegram:`), with `admins` to keep them too (see [Who may use the bot](docs/CONFIGURATION.md#who-may-use-the-bot)).
3. Install as above, skipping signal-cli and its socket.
4. Add the bot to a group and @mention it, or reply to one of its messages.

The bot answers in any group it is in, and a DM from whoever `TELEGRAM_DM_USERS` allows, by default the groups' owners and administrators (of the groups it has seen since it started, by a message there or being added: the Bot API can't list a bot's groups). To stop it being added to groups, send BotFather `/setjoingroups` and choose Disable once it is in yours. Each chat has its own `DAILY_BUDGET`, so keep one set: the more people find the bot, the more it can spend in all. In a group with topics, each topic is its own conversation, with its own history, and the bot answers in the topic it was asked in.

**Privacy mode.** By default Telegram sends a bot in a group only the messages addressed to it, so the bot has no group context. To give it the conversation, send BotFather `/setprivacy` and choose Disable, then remove the bot from the group and add it again; or make it a group admin. As admin it also sees reactions and members joining and leaving.

**On Signal and Telegram at once.** Set both `SIGNALCLI_SOCKET` (even to its default, `127.0.0.1:7583`) and `TELEGRAM_BOT_TOKEN`: one bot then answers on both, each chat on its own network, with one model, one set of features and MCP servers, and one state directory. Each chat keeps its own history and budget, as before; buttons and the keyboard appear only on Telegram, and Signal chats get the same in words. An addon is told both networks, and each call which one its chat is on.

To move from two bots, one for each network, to one, stop both, then fold the Telegram bot's state into the Signal one's (the IDs of the two networks never collide, so each file is a union), and start one bot with both settings:

```sh
sudo bash -s /var/lib/private/signalbot /var/lib/private/signalbot-telegram <<'EOF'   # the Signal bot's state directory, then the Telegram one's
set -e
S=$1 T=$2 own=$(stat -c %u:%g "$1")
merge() { # file, jq program over [Signal's, Telegram's]
  if [ -f "$S/$1" ] && [ -f "$T/$1" ]; then jq -s "$2" "$S/$1" "$T/$1" > "$S/$1.new"
  elif [ -f "$T/$1" ]; then cp "$T/$1" "$S/$1.new"
  else return 0; fi
  chown "$own" "$S/$1.new" && chmod 600 "$S/$1.new" && mv "$S/$1.new" "$S/$1"
}
merge schedules.json '{schedules: ((.[0].schedules // []) + (.[1].schedules // [])), last_id: ((.[0].last_id // {}) + (.[1].last_id // {}))}'
merge keyboards.json '.[0] * .[1]'
merge mcp-feeds.json '.[0] as $a | .[1] as $b | ($a * $b) | with_entries(.value.seen = (($a[.key].seen // []) + ($b[.key].seen // []) | reduce .[] as $i ([]; if index([$i]) then . else . + [$i] end)))'
merge mcp-posts.json '.[0] + .[1]'
EOF
```

A file only the Telegram bot has is copied across, and each file keeps the state directory's owner. Today's spending (`budget.json`) starts afresh for the Telegram chats, and an addon's state there (`mcp.json`: its rounds and pseudonyms) starts over, as the Signal bot's is kept.

Telegram differs from Signal in a few ways: bot chats are not end-to-end encrypted, the Bot API never tells a bot about deleted messages, and a reply is capped at 4,096 characters a message. See [PRIVACY.md](PRIVACY.md#telegram).

### Discord

The same bot can answer on Discord instead, or as well, talking to Discord's gateway and API directly (an outgoing WebSocket, so it needs no public address).

1. In the [developer portal](https://discord.com/developers/applications), choose New Application and name it: the name is the bot's.
2. Under **Bot**, choose Reset Token and copy the token. The token is a password: anyone with it controls the bot.
3. Still under **Bot**, turn on **Message Content Intent** (without it, Discord refuses the bot's connection, and the log says so), and turn off **Public Bot**, so only you can add it to a server (the bot warns at startup while it is on). Turning on **Server Members Intent** too is optional: with it, an @mention in a scheduled or delivered post links anyone in the channel; without it, only people the bot has seen post there since it started (the log says so once).
4. Under **Installation**, set the install link to None (Public Bot can't be turned off otherwise), and under Installation Contexts untick **User Install**, so no one can install the bot on their own account and use its commands without sharing a server with it (the bot warns at startup while it is ticked). Under **OAuth2**, URL Generator, tick the scopes `bot` and `applications.commands`, and the bot permissions View Channels, Send Messages, Send Messages in Threads, Embed Links, Attach Files, Read Message History and Add Reactions; open the URL it makes and add the bot to your server.
5. In `/etc/signalbot.env`, set `DISCORD_BOT_TOKEN` to the token. Leave `SIGNALCLI_SOCKET` unset for Discord alone. Who may DM the bot is `DISCORD_DM_USERS`: unset, only the admins and mods of a server it shares with them; `all` for anyone, `none` for no one, or user IDs (see [Who may use the bot](docs/CONFIGURATION.md#who-may-use-the-bot)).
6. Install as above, skipping signal-cli and its socket.
7. In a channel the bot can see, @mention it or reply to one of its messages; or DM it, or type `/` for its commands.

The bot answers everyone in every channel it can see: the server's roles and channel permissions decide who can reach it there. In a DM it answers only those `DISCORD_DM_USERS` allows, by default a server's owner and those whose roles give Administrator or Manage Server (admins) or Manage Channels, Manage Messages, Moderate Members, Kick Members or Ban Members (mods); anyone else's DM goes unanswered, and the log has only their ID. Each channel, and each thread, is its own conversation, with its own history. Its commands are Discord's slash commands, each with an optional `text`; a slash command's answer replaces Discord's "thinking…". Each chat has its own `DAILY_BUDGET`, so keep one set.

To run on Discord with Signal or Telegram, set each network's setting: one bot then answers on all of them, each chat on its own network, as above.

Discord differs from Signal as Telegram does: bot chats are not end-to-end encrypted, and a reply is capped at 2,000 characters a message. See [PRIVACY.md](PRIVACY.md#discord).

### Slack

The same bot can answer in a Slack workspace, instead of the other networks or as well, through Slack's Web API and Socket Mode (a WebSocket the bot opens, so it needs no public address).

1. At [api.slack.com/apps](https://api.slack.com/apps), choose **Create New App → From a manifest**, pick your workspace, and paste the manifest `signalbot -slack-manifest` prints (from a checkout, `go run . -slack-manifest`; with Docker, `sudo docker compose run --rm signalbot -slack-manifest`). It names the bot, turns on Socket Mode and buttons, and asks for the scopes and events the bot needs.
2. Under **Basic Information → App-Level Tokens**, generate a token with the `connections:write` scope: that is `SLACK_APP_TOKEN`, `xapp-…`.
3. Under **Install App**, install it to the workspace, and copy the **Bot User OAuth Token**: that is `SLACK_BOT_TOKEN`, `xoxb-…`.
4. In `/etc/signalbot.env`, set both. Leave `SIGNALCLI_SOCKET` unset for Slack alone. The tokens are passwords: anyone with them controls the bot. Who may DM the bot is `SLACK_DM_USERS`: unset, only the workspace's owners and admins; `all`, anyone in the workspace; `none`, no one; or member IDs (`U…`, with or without `slack:`), alone or with `admins` (see [Who may use the bot](docs/CONFIGURATION.md#who-may-use-the-bot)).
5. Install as above, skipping signal-cli and its socket, and start the bot.
6. Invite the bot to a channel (`/invite @yourbot`) and @mention it, or DM it.

**Slash commands.** Slack keeps an app's slash commands in its manifest, which the bot can't change itself. At start it writes the manifest, with the commands its features registered, to `slack-manifest.json` in the state directory, and when they changed it logs a line saying to paste it at api.slack.com/apps → your app → **App Manifest**; `signalbot -slack-manifest` prints the one last written, from `STATE_DIRECTORY`, or without it from `/var/lib/private/signalbot` (systemd's; run it with `sudo`), `/data` or the working directory, and says which file it read (before the first start, one without commands, and which files it tried). A command typed in a channel is answered there, and one typed in any DM in the bot's DM with whoever typed it. The bot must be in a channel to answer there.

**Scheduling for a channel.** `/schedule` finds a channel by its name among those you and the bot are both in, and a scheduled post there can @mention anyone in the channel. An app made from an older manifest may lack the scopes for that (`channels:read`, `groups:read` and `users:read`): the bot then logs a line naming them, names channels by their IDs, and knows only the people it has seen post since it started. Paste the manifest again at api.slack.com/apps → your app → **App Manifest**, and reinstall the app when Slack asks.

The bot answers in any channel it is in, and DMs (and slash commands and buttons in a DM) from those `SLACK_DM_USERS` allows, by default the workspace's owners and admins; a refused DM gets no answer, and the log names only the sender's member ID. The workspace's own settings decide who can install, invite and reach it. Each chat has its own `DAILY_BUDGET`, so keep one set. A thread is its own conversation, with its own history: the bot answers in a thread when mentioned there, and any reply in a thread under one of its own messages is addressed to it. Buttons show as Slack buttons, a highlighted one in green; a press's answer shows only to whoever pressed it. Slack has no typing indicator for bots, and no replies outside threads.

Slack differs from Signal in a few ways: messages are not end-to-end encrypted, the bot sees every message in the channels it is in, and a reply is shown in sections of up to 3,000 characters. See [PRIVACY.md](PRIVACY.md#slack).

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
