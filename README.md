# signalbot

A small, privacy conscious chat bot that connects to a running [signal-cli](https://github.com/AsamK/signal-cli) daemon over its socket and answers using the x.ai (Grok) or OpenRouter API.

## Features

- The bot replies when it is @mentioned in a group; people it mentions are shown to the model by real name (every request asks the provider not to keep it, for privacy), and @names in its replies become real Signal mentions
- Replies when someone replies to one of its messages (the quoted text is passed as context)
- Replies to DMs from senders listed in `SIGNAL_DM_USER_IDS`; ignores everything else
- Short answers sized for Signal (aims for one line, never over Signal's 2000-character limit); optional web search and configurable reasoning effort
- Short per-conversation memory so follow-up questions work. This is in-memory only, never logged or stored on disk, and forgotten after a day. Any shorter _disappearing messages_ setting is respected and the message is erased from memory
- Shows a typing indicator (refreshed every 10s) while the bot thinks; many chats are handled at once and a slow answer never blocks others
- The bot ignores everything not addressed to it: a message that doesn't mention it, reply to it, or arrive as a DM is passed over in silence, leaving no trace in its logs, not even a note that it arrived. When it is addressed, it logs who spoke and the token count and cost of each reply

See [TECHNICAL.md](TECHNICAL.md) for how it works.

## Prerequisites

A running [signal-cli](https://github.com/AsamK/signal-cli) daemon with your Signal account registered or linked. How you run it is up to you, but the bot needs only its socket:

- The socket is at `/run/signal-cli/socket` (or set `SIGNALCLI_SOCKET`).
- A group named `signal-cli` can write to the socket and search its directory. The bot's systemd unit joins that group (`SupplementaryGroups=signal-cli`) and Docker does too (`--group-add`). Create it with `sudo groupadd --system signal-cli`.
- signal-cli runs with `UMask=0007`, which makes the socket `0770`. The default umask gives `0755`, which locks the group out. Check with `ls -l /run/signal-cli/socket`.
- signal-cli's data directory holds your account keys, so keep it at mode `0700` (`chmod 700 ~/.local/share/signal-cli`). The umask above makes new files in it group-accessible, so the directory's own mode is what keeps the bot out. Don't run the bot as signal-cli's user.

[`examples/signal-cli.service`](examples/signal-cli.service) is an example unit that does this. Edit `User=` and the path in `ExecStart`, then copy it to `/etc/systemd/system/`, and run `sudo systemctl daemon-reload` and `sudo systemctl enable --now signal-cli`.

To use TCP instead, run signal-cli with `--tcp 127.0.0.1:7583` and set `SIGNALCLI_SOCKET=127.0.0.1:7583`; but then any local user can connect and control or spy on the Signal account (PRIVACY RISK).

## Install

Get the code:

```
git clone https://github.com/robdevops/signalbot.git
cd signalbot
```

Create the config. The file holds your API key, so it is root-only; edit it, setting `XAI_API_KEY` (or `OPENROUTER_API_KEY`) and `SIGNAL_DM_USER_IDS`:

```
sudo install -m 660 .env.example /etc/signalbot.env
sudoedit /etc/signalbot.env
```

Then install the bot with systemd or with Docker.

### With systemd

The bot runs as a dynamic user (`DynamicUser=yes`) in the `signal-cli` group, which is how it reaches the socket.

1. Install Go 1.24+:
   ```
   curl -fsSLO https://go.dev/dl/go1.24.7.linux-amd64.tar.gz
   sudo rm -rf /usr/local/go
   sudo tar -C /usr/local -xzf go1.24.7.linux-amd64.tar.gz
   ```
2. Build and install:
   ```
   /usr/local/go/bin/go build
   sudo install -m 755 signalbot /usr/local/bin/signalbot
   sudo cp signalbot.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now signalbot
   ```
3. Read the logs:
   ```
   journalctl -fu signalbot
   ```

### With Docker

`--group-add` gives the container the `signal-cli` group, which is how it reaches the socket. Docker reads `--env-file` as the calling user, so run it with `sudo`: the file is root-only. Logs go to journald where the host has it, otherwise to Docker's rotating `local` driver; `docker logs` reads both.

```
sudo docker build --build-arg VERSION=$(git describe --tags --always --dirty) -t signalbot .
sudo docker run -d --name signalbot --restart=always --env-file /etc/signalbot.env -v /run/signal-cli:/run/signal-cli \
  --group-add "$(getent group signal-cli | cut -d: -f3)" \
  $(test -S /run/systemd/journal/socket && echo --log-driver=journald || echo --log-driver=local) \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges --pids-limit=64 --memory=300m --memory-swap=300m --ulimit core=0 \
  signalbot
```

Read the logs:

```
docker logs -f signalbot
```

### Then

Add the bot's account to a group and @mention it, reply to one of its messages, or DM it from an allowed sender.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `SIGNALCLI_SOCKET` | required | Unix socket path, or `host:port` for TCP |
| `XAI_API_KEY` | one of the two keys | x.ai API key. Set this or `OPENROUTER_API_KEY`, never both: the bot refuses to start if neither or both are set |
| `OPENROUTER_API_KEY` | one of the two keys | [OpenRouter](https://openrouter.ai) API key, used instead of x.ai |
| `MODEL` | `grok-4.3` (x.ai), `deepseek/deepseek-v4.1-flash-1017` (OpenRouter) | Model name; OpenRouter wants `provider/model`, e.g. `x-ai/grok-4` |
| `REASONING` | `none` | Reasoning effort, e.g. `low`, `medium`, `high` (`none` to omit) |
| `MODEL_SEARCH` | `true` | Let the model search the web, with x.ai's search tool or OpenRouter's web plugin; searches are billed (`true`/`false`, `yes`/`no`, `on`/`off`, `1`/`0`) |
| `SIGNAL_DM_USER_IDS` | empty | UUIDs/numbers allowed to DM the bot, separated by commas or whitespace |
| `SYSTEM_PROMPT` | empty | Extra instructions, appended to the built-in prompt (which keeps answers short and Signal-sized) |
| `INTRO` | built-in sentence | Overrides the self-introduction; `{name}` and `{model}` are substituted |
| `XAI_PRICE_IN` | `1.25` | USD per million input tokens, for the cost estimate |
| `XAI_PRICE_CACHED` | `0.20` | USD per million cached input tokens, for the cost estimate |
| `XAI_PRICE_OUT` | `2.50` | USD per million output tokens, for the cost estimate |
| `MAX_CONCURRENCY` | `32` | Conversations in flight at once; messages beyond this are dropped |
| `HISTORY_TURNS` | `10` | Exchanges remembered per chat |

The default `INTRO` is: `hi, I'm {name}, your open source assistant running {model}. I respond to @mentions and replies to my messages. See https://github.com/robdevops/signalbot/blob/main/README.md for details`

With OpenRouter the cost logged is the amount OpenRouter reports billing, and the `XAI_PRICE_*` variables are ignored.

The x.ai cost estimate uses xAI's published grok-4.3 rates, bills input served from xAI's prompt cache at the cached rate, and adds $5 per 1,000 web searches. It does not model long-prompt pricing tiers, so check [xAI's pricing](https://docs.x.ai/docs/models) and set the `XAI_PRICE_*` variables for other models.
