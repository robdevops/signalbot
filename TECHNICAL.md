# Technical notes

For what happens to your messages and data, see [PRIVACY.md](PRIVACY.md).

## Design

The code is a small core with adapters for chat networks and model providers, and features in their own packages. See [docs/RECODE.md](docs/RECODE.md) for why, and [CONTRIBUTING.md](CONTRIBUTING.md) for the rules that keep it that way.

```
cmd/signalbot/               wiring (main.go) and the feature list (features.go)
internal/bot, chat, history, llm, config, feature   the core
internal/transports/signal   the Signal adapter
internal/llm/openai          the model provider
internal/features/*          example, websearch, charts, mcp, groupctx, vision, intro
internal/kit/*               shared code: logx, txt, img, httpx, bound
```

### Core
- Go, standard library plus `golang.org/x/image` (for chart text, with the bundled Go fonts); single static binary
- The core sees chat networks only as `chat.Message` in and `chat.Outgoing` out, and features only through `feature.Host`. With no features, it still answers, remembers and forgets
- Bounded under load: a concurrency slot (`MAX_CONCURRENCY`) is taken before any goroutine starts, so ignored messages cost nothing and a flood is dropped, not queued. A transport hands over each message cheaply, and anything that may need the network waits until the slot is taken
- In-memory state only, all capped: history per chat (`HISTORY_TURNS`), number of chats (500), names, frame size (1 MiB) and reply length (8,000 characters, split by the network)
- Honours disappearing messages: each message's expiry, derived from the chat's timer, is stored with its history, and no message outlives 24 hours or that timer, whichever is shorter. Once a message expires, reads ignore it at once, it is dropped from the history, and the garbage collector reclaims its memory. Each chat has one expiry timer. When a chat's history is gone, what features and the network kept about it (chart follow-ups, names for mentions) goes too
- Several networks can run at once: chat IDs carry their network (`signal:g:…`), so history and state never mix, and replies go to the network that owns the chat
- The same message repeated by the same sender in the same chat within 10 seconds is ignored
- Message text is never logged, nor anything that can quote it: a panic while handling a message is recovered and reported without its value or stack trace; API error bodies are reduced to their message (OpenRouter's moderation errors quote the flagged input); malformed replies and frames are reported without being echoed. A message that is not a mention, a reply or a DM is not logged at all
- One reply may take at most 5 model requests and 3 minutes; each tool call has 30s, and its result is capped at 16K characters. Tool results are untrusted input to the model, like a quote, and it may follow instructions in them. Only the question and the final answer go into history
- Never swapped: `MemorySwapMax=0` in the unit and `--memory-swap` equal to `--memory` in Docker keep history out of swap (needs cgroup v2 with swap accounting, but no capability)
- Configured from environment variables only; runs under systemd or Docker (hardened unit and image included; the image is pinned by digest and has no shell). Logs one line per event to stdout at four levels (`LOG_LEVEL`: error, warn, info, debug), with a timestamp only in a terminal (journald and Docker add their own). When stdout is a journald stream (systemd's `JOURNAL_STREAM` matches it), each line starts with its syslog priority, such as `<3>` for an error, so journald records the level and `journalctl` colours it; elsewhere, including under Docker, a word such as `error:` marks the level, and errors and warnings go to stderr, the rest to stdout, which Docker's journald driver records at priorities 3 and 6

### Signal adapter (`internal/transports/signal`)
- Talks JSON-RPC to `signal-cli daemon` over a unix socket or TCP (`listAccounts` once per connection to learn its own id)
- Reconnects by itself if signal-cli restarts or the connection drops: it waits 1s, doubling up to 32s between attempts, and resets after a connection has held for a minute. Requests in flight when it drops fail rather than hang
- Decides what is addressed to the bot: a mention, a reply to it, or a DM from `SIGNAL_DM_USER_IDS`; a photo or sticker with no text counts in a DM or a reply to the bot. Unaddressed group messages go to the core (for group context); a DM from anyone else is dropped, and only the sender's ID is logged, never the name or text
- Names come from signal-cli's contact list (`listContacts` with `allRecipients`), loaded on connect and on every group `UPDATE` event (a change of members, name or description all look alike), and replaced wholesale each time. A mention of someone not in the cache reloads it too, as does a mention when the list is over 24 hours old, which catches renames that no event reports. A group `UPDATE` also clears the names remembered for that chat for turning `@Name` into mentions. Reloads are at least 15s apart, and requests made meanwhile fold into one
- Treats sender-controlled text as untrusted: display names and quotes are sanitised before reaching the model or the log, and a name two people claim is never linked
- Splits replies at Signal's 2,000-character limit, and fetches images (`getAttachment`, by `groupId` or by the sender as `recipient`) and stickers (`getSticker`; labelled with their emoji and pack title from `listStickerPacks`, quoted since anyone can make a pack)

### Model provider (`internal/llm/openai`)
- An OpenAI-compatible chat completions client, OpenRouter by default (`MODEL_API_URL`, `MODEL_API_KEY`), over a keep-alive HTTP client that requires TLS 1.3 and retries once on a network error, 429 or 5xx. On OpenRouter, requests set `provider.data_collection: deny` and ask for the real cost to be reported
- The provider's model list says whether MODEL accepts images and can call tools. If it can't call tools, no tools are offered at all

### Features (`internal/features`)
Each feature's package comment describes it in full.
- [`example`](internal/features/example): `/example` is answered by the bot itself, never the model, and leaves no history
- [`websearch`](internal/features/websearch): OpenRouter's web search server tool (`MODEL_SEARCH`)
- [`charts`](internal/features/charts): `make_chart` and `make_donut_chart`, offered when a message asks for a chart (chart, graph, plot, visualise/visualize, pie, donut and their forms) and for the next 2 replies in that chat. Line charts take up to 6 series on one or two Y axes, each with its own mode (values, or % change computed by the bot), unit, title and inversion; a single series is drawn as an area, green if it ends up and red if down, with its high, low and current value marked. Donuts take 2–12 shares, largest first, with the percentages and a shortened total (K, M, B, T) computed by the bot. Charts are 1280×800 PNGs on white. A reply with a chart is one message: the chart, captioned with its title (and subtitle, if both fit on one line) in place of the model's text; history notes each chart sent, by caption
- [`mcp`](internal/features/mcp): tools from MCP servers (`MCP_CONFIG`): remote ones over Streamable HTTP (JSON or server-sent events, with the session re-established if the server forgets it) and local ones over stdio (restarted with backoff if they exit, given only `PATH`, `HOME` and their configured `env`). Each server's `allowed_tools` and `blocked_tools` decide which tools are offered, named `server__tool`. `chat_commands`: `/name text` is a turn in which only that tool is offered, with `tool_choice` requiring the call until its result is in; with `require_arg`, a bare `/name` is answered by the bot. Names are checked at startup (letters, digits, `_`, `-`; free; the tool offered), and skipped with a warning otherwise. Images from MCP tools are attached to the model's text, up to 4 per reply
- [`groupctx`](internal/features/groupctx): a group message not addressed to the bot is added to the group's history as text (`Name: text`, mentions resolved, photos and stickers noted as `[image]`/`[sticker]` but never fetched), never logged, and expires like any other history entry (`GROUP_CONTEXT`)
- [`vision`](internal/features/vision): a message's images, and a quoted message's thumbnails, up to 4 and 5 MB each, and its sticker, are fetched while answering and shown to the model if it accepts images; otherwise the model is told it can't see them. History keeps only a marker, which tells the model it sees images only in the latest message, so to look again someone replies to it and mentions the bot
- [`intro`](internal/features/intro): how the model introduces itself (`INTRO`)

## The prompt

Each request sends the model a system prompt, then the chat's remembered messages, then the new message.

The system prompt is built from these parts, in this order:

1. The network's description and the core's rules, always sent. For Signal:

   > You are a helpful assistant in a Signal chat. Reply in plain text: Signal does not render markdown. Aim for a single line. Two or three sentences at the very most, and only when asked for detail. Never exceed 2000 characters, which is Signal's limit for one message. A mention shown as [unnamed user] is someone whose name you don't know; don't invent one. To mention someone, write @ followed by their exact name. Never write your own name with an @.

2. When the model has tools for this request, a line naming the tool groups the features offer, in feature order (the charts only when they are offered): `Your tools: web search; MCP tools by server, yahoo: get_quote, …. If asked what tools you have, name them, grouped by server.`
3. `SYSTEM_PROMPT`, if set, on a new line after it. It adds to the built-in prompt and never replaces it.
4. Lines features add: the `intro` feature's introduction rule: `If asked to introduce yourself, reply with exactly this text and nothing else: "…"`. The quoted text is `INTRO` with `{name}` (the bot's own Signal profile name, else `signalbot`), `{model}` (without any OpenRouter `provider/` prefix) and `{tools}` (the tool groups by name, e.g. `web search, charts`, or `none`) filled in. It is quoted so a newline or quote in it cannot escape into the rest of the prompt.

The messages that follow are the chat's history (at most `HISTORY_TURNS` exchanges, none older than 24 hours), then the new message. In a group each message is prefixed with its sender's name ("Alice: …"). A quoted message is added as `[Replying to a message attributed to you: "…"]` when it is the bot's own, or as `[Replying to a message by "Bob": "…"]` when someone mentions the bot in reply to another person (`"someone"` if the author's name is unknown). Quotes are escaped and capped at 500 characters, because the sender composes them.

## What the bot needs from signal-cli

Only the socket. The bot speaks JSON-RPC to it and never touches signal-cli's data directory, where the account keys live.

The socket's permissions decide who may connect, and the bot is let in through a Unix group, not a user id, so a transient `DynamicUser` works:

- `signalbot.service` runs with `DynamicUser=yes` and `SupplementaryGroups=signal-cli`, so its throwaway user is in the group.
- Docker's `--group-add` puts the container's user in the same group. The container mounts only the socket's directory, so the data directory isn't there to read.
- signal-cli should create the socket group-writable (`0770`, from `UMask=0007`) in a directory the group can search (`0750`). A default umask gives `0755` and locks out even the group.

Everyone outside the Unix group gets "Permission denied". The group is the only access control: anyone in it can drive the Signal account, so keep it to the bot.

The bot's keys stay out of reach in two ways. It runs as a different user from signal-cli, and the data directory is mode `0700`, so it can't be searched. That mode matters: `UMask=0007` makes files signal-cli creates group-accessible, and the bot is in that group, so with a `0755` directory it could read them (tested). Also, `ProtectHome=yes` hides `/home`, where signal-cli keeps its data by default (`~/.local/share/signal-cli`). Data kept elsewhere, such as `/var/lib/signal-cli`, relies on the directory mode alone; add it to `InaccessiblePaths=` in the unit to hide it outright.

A TCP socket (`--tcp 127.0.0.1:7583`) needs none of this, but then any local user can connect and control or spy on the Signal account (PRIVACY RISK).
