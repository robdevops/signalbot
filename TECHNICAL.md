# Technical notes

How the bot is built. For what happens to your messages and data, see [PRIVACY.md](PRIVACY.md).

Go, the standard library plus `golang.org/x/image` (for chart text, with the bundled Go fonts); one static binary.

## Design rules

- **A core written from a spec.** It is lightweight, scales with load, and handles many chats concurrently. With no features it still answers messages (below).
- **Features bolted on.** Each feature is a package that registers on the core's ports (`internal/feature`). `features.go` lists them, one line each.
- **The core never mirrors a feature.** Delete any feature and the core doesn't change. If a feature can't be built on the ports, either a port becomes more generic (and stays tied to its stage), or the feature takes on more of the work. A feature-shaped field in the core is never the answer.
- **Plain Go, no ceremony.** Concrete types and structs. An interface only where there are two real implementations, or a test needs a fake. Each type defined once and used everywhere. Many inputs go in a typed struct, never a map or a long list of positional arguments. No wrappers or adapters that only pass calls through.
- **Scope.** Signal only, kept out of the core behind a small interface ([Layout](#layout)), so the core doesn't preclude other chat networks. The model is any chat completions API in OpenAI's format, OpenRouter's by default, or none at all.

## The core

What the bot does with no features registered.

- **Receive.** JSON-RPC to `signal-cli daemon` over a unix socket or TCP (`listAccounts` once per connection to learn its own ID). If the connection drops it reconnects, waiting 1s, doubling up to 32s, reset once a connection has held for a minute; requests in flight fail rather than hang. Frames are capped at 17 MiB, room for a 12 MB image in base64; a longer frame is skipped with a warning and the request it answers (found by the `id` at its end) fails, but the connection is kept. The bot's own messages are ignored.
- **Addressed or not.** A message is answered if it mentions the bot, replies to one of its messages, or is a DM from a sender in `SIGNAL_DM_USER_IDS`, and it carries text, an attachment signal-cli can hand over (an ID, up to 12 MB: an image or any other file) or a sticker; anything else is never answered. A DM from anyone else is dropped and only the sender's ID is logged. An unaddressed group message goes to the features and is never logged.
- **Names** come from signal-cli's contact list (`listContacts` with `allRecipients`, at most 5,000), loaded on connect, on every group `UPDATE` event, and when a mention is unknown or the list is over 24 hours old, at most every 15s, with requests meanwhile folded into one. A group `UPDATE` also clears the names that chat remembers for turning `@Name` into mentions; it is a change, not a message, so no hook sees it, and it never takes a slot, even before the bot's own ID is known. An unresolved mention reaches the model as `[unnamed user]`; a group sender with no known name gets no prefix; an unknown quote author is "someone". The bot calls itself by its profile name, or `signalbot`. A name two people claim is never linked.
- **Answer.** Typing is shown until the reply is ready. The prompt is built ([below](#the-prompt)) and sent to the model. On failure the "model error" line is logged, the chat gets "Sorry, I couldn't get an answer right now.", and history is unchanged. The network renders the reply's Markdown, then sends it without a leading @ and the bot's own name (an echo of how it was addressed), with @Name, and in a group the bot's own @name elsewhere (as in the intro's `I respond to @{name}`), linked to real mentions, split at 2,000 characters (preferring a line break, then a space) and capped at 8,000 in all, the rest cut with " […truncated]" and a warning.
- **Markdown on Signal.** Signal shows text styles, not Markdown, so `internal/signal/markdown.go` turns a reply into plain text and signal-cli's `textStyle` ranges (`start:length:STYLE`, UTF-16 units of each message sent): `**bold**` and `__bold__` are BOLD, `*italic*` and `_italic_` ITALIC, `~~strike~~` STRIKETHROUGH, `` `code` `` and fenced code blocks (fences and language dropped) MONOSPACE, `||spoiler||` SPOILER; `# Heading` (1–6 `#`) is its text in bold, and `[text](url)` is "text (url)", or the url alone when they are the same. Markers open and close only at word boundaries (`2*3*4`, `snake_case_name` and `a * b` stay as they are), an unmatched one stays literal, nothing in code is converted, and lists, tables and quotes stay as written. Conversion comes first, so stripping, splitting and mentions work on the plain text, and a style that spans a split carries on in the next message.
- **Nothing to answer.** A message with no text (only a photo or sticker) is answered only if an `OnTurn` hook adds something to it.
- **The model.** A chat completions API in OpenAI's format at `MODEL_API_URL` (OpenRouter's by default, `internal/model`), with `MODEL_API_KEY` as a bearer token if set, over a keep-alive client that requires TLS 1.3 for `https://`. Requests to OpenRouter (host `openrouter.ai`) set `provider.data_collection: deny` and `usage.include` for the real cost, plus the `MODEL_REASONING` effort as `reasoning.effort`; requests elsewhere carry only `reasoning_effort`. Either effort is left out when `MODEL_REASONING` is `none`. The tokens are logged with each reply, and the cost when the API reports it. A network error, 429 or 5xx gets one retry; an error body is reduced to its message ("status N: message"). The tool loop takes at most 5 requests and 3 minutes per reply, and 4 pictures.
- **Recent attachments.** signal-cli gives a quote only thumbnails, with no IDs for the quoted message's files. So the client remembers each received group message's attachments, and those of DMs from allowed senders, by chat, author and timestamp: IDs, names, types and sizes, never content, in memory, for at most a day and 2,000 messages. A reply's quote (its `id` is the quoted message's timestamp) then gets the original images and files; one not remembered gets the thumbnails, and its files are passed with no ID, so they are noted as no longer available.
- **Capabilities.** At startup OpenRouter's model list says whether the model takes images and audio, reads files natively (`file`), and can call tools; PDFs are always taken there, since OpenRouter reads one for a model that can't (see the files feature); tools are offered only if it can (otherwise one warning says "tools are off"). If the check fails or the model isn't listed, images are off and tools stay on. Any other API has no model list, so both are assumed (one startup line says so) until it rejects them: when the first request of a reply carries images and gets a 400 or 422 (the API calling the request malformed; other 4xx codes are about the key, credit, rate, model id, a timeout or a size, and stay plain errors), the client returns `model.ErrImages` (else, if it carried files, `model.ErrFiles`, which turns off audio too; else, if it carried tools, `model.ErrTools`) without retrying, and the core turns that capability off for the rest of the run, with one warning, and asks the same turn again without it, hooks included, so `vision` then tells the model it can't see the images. A request with both is blamed on its images first; if the turn turned images off and the request without them is then rejected for its tools, images go back on (one info line), since they weren't the problem. A turn is asked at most four times, since with images off none are sent, with tools off none are offered, and images come back once. Once tools are off, a chat command (which forces a tool) is answered "That command needs tools, which this model can't use." without asking the model, rather than answered without its tool's data. On OpenRouter a 4xx is just an error. A server tool (`Tool.Server`, `provider:name`) is offered only when the API is that provider's (`openrouter` for OpenRouter); otherwise it is left out with one startup warning naming it, and a tool group left with no tools is dropped. The core never names a server tool.
- **No model.** With neither `MODEL_API_URL` nor `MODEL_API_KEY` set, startup logs "no model: only commands are answered" and checks no capabilities: `Info.Model` is "", and images and tool calling are off. A message a hook answers (a command) is answered as usual, and a delivered plain text is posted; anything else addressed gets no reply, no typing and no model request, so `OnTurn` and tool groups are never used. No history is kept at all: no turns, no `Context` from unaddressed messages, no delivered text.
- **History.** Messages not addressed to the bot (a `Context` from an `OnMessage` hook, or a scheduled post) are kept marked as context, and each run of them between the bot's exchanges reaches the model as one user message opening `[Messages not addressed to you, for context only:]`, one line each, so others' talk reads as background, not as questions each wanting an answer. Per chat, at most the latest `HISTORY_MAX` messages (each message to the bot, each reply, and in a group each message kept as context counts as one), in memory only. A message that takes it over the cap drops it to the latest half (rounded up), not just the oldest one: the model API's prompt cache only reuses the unchanged start of a request (tool definitions, system prompt, then history), so a history that lost its oldest message every time would be re-read in full with every message once full, while one that drops in steps keeps the same start for several messages. Under 3 it drops one at a time. It is kept for at most 500 chats. Each entry expires after 24 hours, or sooner when the chat's disappearing-messages timer (`expiresInSeconds`) runs out, on one `time.AfterFunc` per chat set for its earliest entry. When a chat's history is gone, everything kept about it goes too: its remembered names and tool follow-up counts.
- **Safe under load.** Concurrency, mailboxes, history, chats, names, timers and frames are capped. The same message from the same sender in the same chat within 10 seconds is dropped. Display names and quotes are sanitised before they reach the model or the log.
- **Logging.** By journald priority, never names, IDs or message bodies; see [Logs](#logs).
- **Configuration** is environment variables only; `-version` prints the version. Runs under systemd or Docker (hardened unit and image included; the image is pinned by digest and has no shell). `MemorySwapMax=0` in the unit and `--memory-swap` equal to `--memory` in Docker keep history out of swap.

## Logs

Each line has a journald priority, and every priority is always written:

| Priority | Used for |
|---|---|
| `<2>` crit | errors that stop the bot, such as a bad config |
| `<3>` err | errors, such as a failed model request |
| `<4>` warning | warnings, such as a dropped message under load |
| `<5>` notice | startup, connecting and reconnecting |
| `<6>` info | each message answered ("received group chat" or "received dm", then the reply's time, tokens and cost), tool calls |
| `<7>` debug | detail, such as contact list loads |

- **Streams.** Priorities 5–7 go to stdout and 2–4 to stderr, so a log driver that sees only the stream still tells them apart. Under journald (`JOURNAL_STREAM` matches stdout) each line carries its `<N>` prefix, so `journalctl -p warning` filters by level; elsewhere, as in Docker, lines are plain, with a timestamp only in a terminal. Docker's journald driver records stderr at priority 3 and stdout at 6.
- **What's never logged.** No line holds anyone's name or ID, except the ID of a sender not in `SIGNAL_DM_USER_IDS` whose DM was dropped; the bot's own account ID is logged at startup. Names and message bodies are never logged: a panic while handling a message is recovered and reported without its value or stack; API error bodies are reduced to their message; malformed frames are reported by size. The one exception is a chart tool's argument error, logged with the reason the model is given (e.g. `source must be a string, got object`), which can quote a series name or a date from the model's arguments. Every logged value has `\n` and `\r` escaped, so no value can start a forged line.

## Concurrency and scale

- **Reader goroutine.** The Signal client's reader parses each frame and decides "addressed?" without blocking or calling the network, then hands the bot a `bot.Received`: an addressed message (with a `Load` to complete it in its slot), an unaddressed group message, a group change, or, before the bot's own ID is known, a message to wait, also with a `Load`. Only a message with a `Load` goes to its chat's mailbox from here, and a delivered one is the only one there without. The bot may briefly take the chat store's lock (to queue a message or keep context). Unaddressed group messages go to the `OnMessage` hooks here.
- **Worker slots.** A fixed pool of `MAX_CONCURRENCY`. Message handling that may call the network (waiting for the account ID, contacts a message needs, sticker labels, media, the model) runs only in a slot. The contact reloads on connect and on group changes run on their own timer, one at a time, and `listAccounts` on each connection's own goroutine. Until the bot's own ID is known, a message waits for it in a slot, up to 10s. A full pool drops the message with a warning.
- **Per-chat mailbox.** An addressed message, one a feature delivers, or one waiting for the bot's own ID joins its chat's mailbox (`chats.Store.Post`, generic over what it holds, so chats needn't know the bot's type); at most 4 wait, and more are dropped with a warning. A chat with work has one worker, holding one slot, which takes everything waiting at once, handles it in order, and exits when nothing is left, so each answer sees the ones before it. Different chats run in parallel.
- **Answered together.** Messages that arrive while the worker is busy are answered as one turn, a line each (in a group with the sender's name), so "actually" and "forget it" get one reply that sees both. On its own, in order, goes a message a hook acts on (such as a command), a delivered one, and one that waited for the bot's ID and was not addressed after all; a run is split where it would carry more than 4 images or a second sticker. A panic loses only the message or turn that caused it, and once shutdown begins nothing more that waited starts, not even its hooks.
- **Per-chat state.** One struct per chat (`internal/chats`), in one capped map: the history and its expiry timer, recent message keys (for dedupe), mention names, the tool groups' follow-up counts, and the mailbox. One lock guards the whole map and is held only to read or change it, never across a model call. The struct is deleted, or evicted when the map is full, only once its mailbox is empty and no worker is active, so a chat never gets a second worker.
- **HTTP.** The model client keeps up to `MAX_CONCURRENCY` idle connections (Go's default is 2 per host).
- **Shutdown.** On SIGTERM or SIGINT, timers and new work stop, work in flight drains for up to 8s (under Docker's 10s stop timeout), then the socket closes and the bot waits for what features run with `Go`. A second signal kills the process at once.

## Layout

```
main.go, features.go    the main package at the module root: wiring, and one line per feature
internal/signal/        the chat network, outside the core: its settings, the socket, decoding, sending (Markdown to text styles), names; only main imports it, and it imports bot
internal/model/         the model client (OpenRouter or another chat completions API) and the tool loop
internal/chats/         per-chat state: history, dedupe, mention names, the mailbox
internal/timers/        one-shot timers
internal/config/        the core's settings from the environment; config.Test, the test hooks
internal/bot/           the decision, the prompt, the hooks; wires the core packages, and reaches the network through Network
internal/feature/       the ports and shared types (the only part of the core features import), the command helper, featuretest
internal/features/*     one package per feature
internal/kit/*          code with two or more users and no bot knowledge
internal/arch/          the guardrail tests and the sizes report
```

- Core packages are concrete and call each other directly, with no interfaces between them. Files by role, no line limit.
- **The network seam.** The bot reaches Signal only through `bot.Network`; `*signal.Client` implements it, only main imports `internal/signal`, and the interface exists for the bot's in-memory test fake. The network depends on the core, never the reverse: `internal/signal` imports `internal/bot` for `Received`, which sits next to `Network`. The core's prompt names no network: Signal's line, with its 2,000-character limit, comes from `Network.Prompt`, and the golden prompts pin it.
- **`bot.Received`** is a message as the network hands it to the core, or as a feature delivers it, and what a chat's mailbox holds. Features never see one, so it isn't in `internal/feature`. `Network.Delivered(ctx, msg)` returns one and ok: the message with its sender's name and without the bot's @name, `Mentioned` if that was there, and in a group its `Members` (name and ID), which the core claims into the names a post links.
- **Why `Send` takes the names to link.** The chat's `@Name`-to-ID map is per-chat state, so it lives in `internal/chats` with the rest: learned from the names the model was shown, cleared on a group change, gone when the history goes, and with one rule for a name two people claim (`chats.Claim`). The network keeps no per-chat state, and only the core knows which map a send needs: the chat's for a reply, the group's members for a delivered post. So `Send` takes it as an argument rather than the network keeping a copy.
- **Why the caption rule is the core's.** When a reply's pictures have captions, the core sends those in place of the model's text, and history keeps only the pictures' notes. It decides what history keeps, which is core state, and it happens after the model answers, where no port reaches; a feature doing it would need a fourth stage for this alone. `Picture.Caption` is generic (any tool's picture may have one), so the rule stays a few lines in the core.
- `Message`, `Media`, `Picture` and the port types are defined once, in `internal/feature`, and the core uses them directly: nothing is copied or converted.
- **Picture file names.** A `Picture`'s `Name` is the file name it is sent with, without the extension: a chart's title, or `line_`/`pie_` and the local time (`pie_20261101-223115`) if the title has no letter or digit. The network makes it safe (Signal drops `/\:*?"<>|;,` and control characters and cuts it to 80 characters), and names a picture with none `image_<local time>`, adding `_2`, `_3` for further pictures in the same message.
- Kit holds what both the core and features use: `logx`, `txt`, `img` and `httpx`. It gains a package only when a second user appears, and never imports the core.

## Lifecycle and ports

The core's lifecycle has three stages. Each port is one stage; nothing else is a port.

| Stage | Port | Returns |
|---|---|---|
| A message arrives | `OnMessage(func(ctx, Message) Action)` | carry on, answer with text without the model, choose a tool, or keep text as context |
| The model is asked | `OnTurn(func(ctx, Message) Additions)`, plus `Tools(ToolGroup)` | a note on the message (with its own version for history), and images |
| Time arrives | `At` ([Timers](#timers)) | nothing: the timer may deliver a message |

**Hook rules:**
- Hooks run in feature order (`features.go`).
- For an unaddressed message, `OnMessage` runs on the reader goroutine, so it must not block or call the network. For an addressed or delivered one it runs in the message's slot. The first non-zero `Action` wins.
- `Answer` and `ToolChoice` count only on an addressed message, and `Context` only on an unaddressed one.
- `OnTurn` runs only when the model is about to be asked.

**Services the host offers:**
- `IntroLine(line, dmOnly)`: a paragraph for the bot's introduction, placed where `INTRO` has `{more}`; with `dmOnly`, only in the introduction a DM gets (`Info.IntroDM`, and the prompt's rule in a DM), not a group's (`Info.Intro`). The core pads each with a blank line and never leaves two blank lines in a row. `/schedule` uses it, DM only, for `Type "/schedule" for schedules and reminders.`, since schedules are made only in a DM.
- `Env(key, def)`: a feature's own setting. The docs guardrail finds each name passed to it.
- `Info()`: the bot's name, its version, the model ("" with no model), whether it takes images and can call tools, and the names of the tools, as in `{tools}` (none when it can't call tools). All but the name and the tools are known when features register; images and tool calling may turn off later ([Capabilities](#the-core)).
- `Media(ctx, msg, i)`: the bytes of a message's image or sticker (15s). The core fills in each sticker's `Label` (its emoji and pack title) when it loads the message.
- `Deliver(msg)`: act on a message as if its sender had just posted it in its chat, through the chat's mailbox. Inside the message's slot:
  - it waits up to 10s for the bot to be connected, else it is dropped with a warning;
  - in a DM, the sender must still be in `SIGNAL_DM_USER_IDS`; in a group, still a member (signal-cli's `listGroups`). Otherwise it is dropped and logged at info, with no content;
  - the bot counts as mentioned when the text has `@` followed by its profile name or `signalbot`, ignoring capitals, on a word boundary; that @name is stripped as for a real mention;
  - a command or a mention of the bot is handled like any addressed message;
  - anything else the bot posts as written, with @Name linked against the group's members (never an ambiguous name), and history keeps it as the sender's message, "<name>: <text>", not as something the bot chose to say.

  These rules are what "as if its sender had just posted it" means for any feature, not just `/schedule`, so the host applies them rather than each feature. The membership check calls the network, so it runs in the message's slot; a feature's timer callback has no slot to make that call in.
- `Groups(ctx, user)`: the groups the bot shares with a user, with their names (`listGroups`).
- `Redact(text)`: text with every name and ID the bot knows for anyone but itself, and any UUID or phone number, replaced with "redacted", for logging text the bot doesn't control (a local MCP server's output).
- `Go(fn)`: run `fn` for as long as the bot runs. Its ctx ends when the bot stops, which then waits for `fn` to return (MCP stops its local servers this way).

**Types**, each field being a stage's natural input or output:
```go
type Action    struct{ Answer, ToolChoice, Context string } // zero value: carry on
type Additions struct{ Note, HistoryNote string; Images []string; Files []File }
type File      struct{ Name, MIME string; Data []byte }
type Picture   struct{ Data []byte; Caption, HistoryNote string }
type ToolGroup struct{ Names []string; Listing, System string; Tools []Tool; Keywords *regexp.Regexp }
type Tool      struct{ Name, Description string; Schema json.RawMessage; Server string; Run func(ctx context.Context, args string) (string, []Picture, error) }
```
- **`Message`** is a message in a chat: sender, name, text (mentions as `@Name` or `[unnamed user]`), quote, media (its images, then the quoted message's, at most 4; then its other files and the quoted message's, at most 4; then its sticker), the disappearing-messages timer, and whether it is addressed. `OnTurn` gets the message being answered. A `Media` is an image, another file (`File`, with the `Name`, `MIME` and `Size` its sender's app gave, unchecked) or a sticker, by an ID only the network reads; a quoted file the network couldn't find has no ID.
- **`Tool`:** a function tool has `Schema` and `Run`, which returns text and any pictures. A server tool, run by the model's provider itself, has only `Server`, as `provider:name` (for example `openrouter:web_search`).
- **`ToolChoice`** is the model API's own concept, not a chat-command field: that tool is required through `tool_choice` until its result is in, then released.
- **`Context`** is "keep this text in history", which the core does under its own expiry rules.
- **A picture's `HistoryNote`** is set by the tool that made it; without one, the core records "[sent an image]". When pictures have captions, those are sent, one per line, in place of the model's text, which history then doesn't keep either.
- **`ToolGroup`:** `Names` is what it adds to the intro's `{tools}` (and so to `Info.Tools` and `/intro`) and to the prompt's `If asked what tools you have:` list; the core joins every group's names in feature order. Each is Markdown with a bold label, such as `**web search**` or `**charts**: line, pie`, so the network renders the label in bold. The intro's `{tools}` puts each on its own paragraph; the prompt's list separates them with semicolons. `Listing` is its item in the tools line, plain text. `System` is a system-prompt line while the group is offered. A group with tools, none of which the model's API can run, is dropped. A group with no `Tools` at all, only `Names`, is kept for the prompt's list alone, not `{tools}`: it is how a feature tells the model about chat commands that use no model tool (`/schedule` and its aliases, which the intro already mentions in a line of their own).

**Offering:** a group without `Keywords` is offered with every message. One with `Keywords` is offered when what was typed matches (never a name or a quote), and for the `TOOL_LINGER_ROUNDS` replies (default 2, 0 to `HISTORY_MAX`) after any reply that matched or used one of its tools; each match or use starts the count again. A `ToolChoice` (a chat command) adds its tool to whatever else is offered, even from a group whose keywords didn't match, and the model must call it before it answers.

**Commands** are a helper in `internal/feature` built on `OnMessage`: `feature.Command(h, name, fn)` matches an addressed message whose text is `/name` or `/name args`, the name in any case. A name is 1 to 32 lowercase letters, digits, `_` or `-`, and one another feature has taken is refused at registration. The core doesn't list commands: a feature names its own in its tool group's `Names` and `Listing`.

## The prompt

Each request sends a system prompt, then the chat's history, then the new message.

The system prompt is, in order:

1. The built-in prompt, with the network's own line (`Network.Prompt`, here Signal's) after the brevity rules and the latest-message rule, which keeps the model from answering everything said since its last reply (in a group, history holds messages not addressed to it):

   > You are a helpful assistant in a chat. Reply in Markdown. Aim for a single line. Two or three sentences at the very most, and only when asked for detail. Earlier messages are the conversation so far, not questions for you: reply to the latest message alone, using them only to understand it or when it asks you to. This is a Signal chat. Never exceed 2000 characters, which is Signal's limit for one message. A mention shown as [unnamed user] is someone whose name you don't know; don't invent one. To mention someone, write @ followed by their exact name. Never write your own name with an @.

   When the model can call tools and any are registered, one more sentence follows: `If your answer uses a tool or search, name the source briefly, e.g. "(Yahoo Finance)", or give its url when that helps.` It names no tool, so the core stays feature-free.

2. When the model can call tools and any are registered, the tools line: `Your tools: ` and the listing of each tool group offered for this message, then `If asked what tools you have:` and the names of every group, offered or not, including those with only `Names`. For example: `Your tools: web search; charts (make_chart, make_pie_chart); weather: get_forecast, /forecast. If asked what tools you have: **web search**; **charts**: line, pie; **weather**: get_forecast, /forecast; **schedules and reminders**: /schedule, /timer, /reminder, /job.`
3. The `System` line of each offered tool group (an MCP server's `instructions`).
4. `MODEL_SYSTEM_PROMPT`, if set. It adds to the built-in prompt and never replaces it.
5. The introduction rule, always last: `If asked to introduce yourself, reply with exactly this text and nothing else: "…"`. The quoted text is `INTRO` with `{name}` (the bot's profile name, else `signalbot`), `{model}` (without the `provider/` prefix) and `{tools}` (every group's names in feature order, or `none` when there are none or the model can't call tools) and `{more}` (features' lines, the DM-only ones only in a DM) filled in. It is quoted so a newline or quote in it can't escape into the rest of the prompt.

With no tool groups, as with no features, there is no tools line and `{tools}` is `none`, whatever the model can do.

In a group each message is prefixed with its sender's name ("Alice: …"). A quote is added as `[Replying to a message attributed to you: "…"]` when it is the bot's own, or `[Replying to a message by "Bob": "…"]` otherwise. Quotes are escaped and capped at 500 characters, because the sender composes them. A picture sent with a reply is kept in history as its note (for example `[sent the chart "NVDA"]`), or `[sent an image]`.

## Timers

Time is a source of work, like a message, so timers are in the core (`internal/timers`); `/schedule` is a feature on top of them.

```go
At(name string, t time.Time, fn func(ctx context.Context, due time.Time)) (cancel func(), err error)
```

- **One-shot:** a timer runs once and is removed. A feature that repeats sets its next timer when one runs, so the core needs no cron.
- **Always called:** `fn` always runs, however late, and is passed the time it was due. The feature decides what a late run means, so a late run never stops a repeating schedule.
- **One goroutine** keeps every timer in a min-heap. It sleeps until the next is due, but at most a minute, and compares due times with the wall clock, so a suspend or clock jump is noticed within a minute.
- **No slot:** a callback does only cheap work, such as setting the next timer and delivering a message. The delivered message takes a slot like any other: if the pool or the chat's mailbox is full, that run is dropped with a warning, and a repeating schedule keeps going.
- **Caps:** at most 10,000 timers; `At` returns an error at the cap.
- **Start and stop:** timers run once the bot first connects to signal-cli, so a timer due at startup waits for it. They live in memory only; at shutdown the rest are dropped and `At` fails. A feature that saves its own (`/schedule`) sets them again at startup.
- **Time zone:** the local one. Go reads `TZ` itself (UTC in the container), and the image ships tzdata.
- **Logs:** each run logs the timer's name and how long it took, at debug. A panic in a callback is recovered and logged.

## Features

In `features.go` order, which is the order hooks run in and of the tools line and `{tools}` (`websearch`, `charts`, then `mcp`).

| Feature | Ports and services |
|---|---|
| `example` | command helper, `Info` |
| `websearch` | `Tools`, `Info` |
| `groupctx` | `OnMessage` returning `Context` |
| `vision` | `OnTurn`, `Info`, `Media` |
| `files` | `OnTurn` returning `Files`, `Info`, `Media` |
| `charts` | `Tools` with `Keywords` |
| `mcp` | `Tools` (one group per server), command helper returning `ToolChoice`, `Info`, `Redact`, `Go` |
| `schedule` | command helper, `At`, `Deliver`, `Groups`, `Info` |

- **`intro`** answers `/intro`, and its alias `/help`, itself, never through the model, with the bot's introduction: `INTRO` with `{name}`, `{model}` (or "no model"), `{tools}` and `{more}` filled in by the core (`Info.IntroDM` in a DM, `Info.Intro` in a group), the same text the model gives when asked to introduce itself, followed by "This message did not use the model." The tool labels are Markdown, so they arrive in bold. It leaves no history. `/intro` or `/help` with anything after it carries on as a normal message.
- **`websearch`** offers OpenRouter's server-side web search tool (`openrouter:web_search`), which the model calls only when it decides to, unless `MODEL_SEARCH` is off. Its `System` line asks the model to cite the sites it used by url. It logs "web search on" only when the core kept the tool: on another API the core leaves it out and warns ([Capabilities](#the-core)), and with no model nothing is offered or logged.
- **`groupctx`** (`GROUP_CONTEXT`, on by default) keeps each unaddressed group message as context: `Name: text`, capped at 2,000 characters, with photos, stickers and files noted as `[image]`, `[sticker]` and `[file "name"]` but never fetched. The core keeps it like any history entry, marked as context, and never logs it.
- **`vision`** adds a message's images (and a quoted message's thumbnails), up to 4 and 12 MB each, fetched with `getAttachment`. One over the model's 5 MB becomes a JPEG: turned upright by its EXIF orientation, shrunk by averaging blocks of pixels to at most 2,048 a side, on white. Only one is shrunk at a time, and one that would decode to over 64 MB (about 21 megapixels of JPEG) is refused, so a photo takes tens of MB, not hundreds, under the unit's 300 MB. and its sticker, fetched with `getSticker` and labelled with its emoji and pack title from `listStickerPacks` (quoted, since anyone can make a pack). If the model takes images they go as image parts; otherwise the model is told it can't see them, and a sticker still gets its label. One that can't be fetched or shrunk is left out, logged, and noted as having failed to load ("you can see images, just not this one"), so the model doesn't claim it can't see images. So a photo or sticker alone is answered in a DM or a reply to the bot. History keeps only a note, which tells the model it sees images only in the latest message, so to look again someone replies to it and mentions the bot.
- **`files`** handles each `Media` with `File` set, fetched with `getAttachment`, by its content, extension and declared type. A PDF (`%PDF-`) goes to the model as a `File` when `Info.Files`, which the model client sends as a `file` part; on OpenRouter, for a model whose list entry lacks `file`, the request also names the `file-parser` plugin with the `mistral-ocr` engine, named rather than left to OpenRouter's default so what is billed is predictable. Audio goes as an `input_audio` part (format from its type, such as `aac` for Signal's voice notes) when `Info.Audio`. Text (valid UTF-8, no NUL, by type or extension) and docx, xlsx and pptx (text extracted with `archive/zip` and `encoding/xml`, each part read to at most 10 MB) are shown inline in a Markdown fence longer than any backticks in them, capped at 50,000 characters. Anything else, or a file that fails to load or can't be read, is noted as such. Names come from the sender, so they are made safe and quoted. History keeps only the notes, saying the file isn't kept. OpenRouter's `annotations` on a reply (such as a parsed PDF) are sent back with it in later tool rounds, so the PDF isn't parsed again within the reply.
- **`charts`** offers `make_chart` and `make_pie_chart` when a message asks for a chart (chart, graph, plot, visualise/visualize, pie, donut and their forms) or for what a line chart shows (projection, forward estimate, trending, forecast and their forms). `make_chart` draws line charts of up to 6 series on one Y axis, set by a top-level `y_axis` (an axis given as just its title, `"y_axis": "…"` or `"axes": {"label": "…"}`, is taken as meant), and refuses a right axis, saying to use change mode for series on different scales. A second axis is only on request: `make_dual_axis_chart`, the same chart with a left and a right Y axis, is a tool group of its own (no `Names`, so the intro is unchanged), offered only when a message asks for one (second, secondary, dual, two, right-hand… axis or axes), and for the replies that follow. Each axis has its own mode (values as given, the default, or % change from each series' first point, for comparing series on different scales such as two stocks), unit, title and optional inversion; a single series is an area, green if it ends up and red if down, with its high, low and current value marked. `make_pie_chart` draws 2–12 shares of a total as a ring (its description still says donut), largest first, the legend giving each amount and the centre the total, both shortened (K, M, B, T) by the bot; a unit or suffix over 4 characters (such as a word) is dropped rather than cut. Both take an optional `source` (where the data came from, trimmed and capped at 80 characters), written as "Source: …" small and muted at the bottom left and cut with "…" to the chart's width, in a band below the date labels and the legend, whose row gaps shrink to stay above it (and if that is not enough, the smallest slices' labels go back to one line, cut before their amount); it is not in the caption. Charts are 1280×800 PNGs on white (at most 256 colours: anti-aliased shades are grouped and the theme's colours kept exact, for about a third the size), or with `DARKMODE` on in a dark theme (near-black navy, light text, a lighter palette); `auto` is dark from 21:00 to 07:00 local time (`TZ`), decided as each chart is drawn, and any other value is off with a warning at startup. A reply with a chart is the chart alone, captioned with its title (and subtitle, if both fit on one line) in place of the model's text; history notes it by caption. Arguments sent as a string holding their JSON, whole or for one field (such as `axes` or one series' `points`), are taken as meant, as smaller models often send them; any other mistake is named back to the model, with how to fix it, so it can correct its call.
- **`mcp`** (`MCP_CONFIG`) connects to MCP servers: remote ones (`url`, `headers`) over Streamable HTTP (JSON or server-sent events, with the session re-established if the server forgets it) and local ones (`command`, `args`, `env`) over stdio (restarted with backoff if they exit, given only `PATH`, `HOME` and their `env`, and stopped when the bot stops; each line a local server writes to stderr is logged at debug only after `Redact`). It identifies itself in the handshake as `signalbot` with the bot's version (`Info.Version`, or `dev`). A server with `disabled` is skipped unchecked; one that can't be reached is logged and left out. At startup it logs the tools it offers ("MCP tools: …") and the chat commands ("chat commands: …").
  - Each server is one tool group, listed as `server: tool, …, /command`. In the intro's `{tools}` and the prompt's `If asked what tools you have:` list its label is `**server MCP**: tool, …, /command`, without the added ` MCP` when the server's name already contains MCP in any case. `allowed_tools` or `blocked_tools` decide its tools (a name in either that the server lacks stops the bot), named `server__tool` for the model. `instructions` is its system-prompt line while offered: the config's if set (even to `""`), otherwise the `instructions` the server sent in the MCP handshake, capped at 1,000 characters. `keywords` (whole words, any case) gate it like the chart tools.
  - Each call has 30s and its text result is capped at 16K characters; images in a result are sent with the reply, up to 4. Tool results are untrusted input to the model, like a quote. Only the question and the final answer go into history; arguments and results are never logged.
  - `chat_commands` make `/name text` a turn in which that `tool` is added to whatever else is offered and must be called first; with `require_arg`, a bare `/name` is answered by the bot. A command whose name is invalid or taken, or whose tool isn't offered, is skipped with a warning; none is registered when the model can't call tools. A server's group adds one entry to `{tools}`, grouped by server: its name in bold, a colon, then its tools and its own `/commands`, sorted, such as `**weather**: get_forecast, /forecast`. The tools line lists it as `weather: get_forecast, /forecast`.
  - With no model it connects to no server and logs "MCP off: no model".
- **`schedule`** is `/schedule` ([below](#schedule)).

### `/schedule`

A user schedules anything they could post in the chat. The user guide is in the [README](README.md#scheduling). `/timer`, `/reminder` and `/job` are aliases. All four are named, with no tools, in the prompt's `If asked what tools you have:` list as `**schedules and reminders**: /schedule, /timer, /reminder, /job`, so the model can point a near-miss such as `/time` to them; there is no `/time` alias. It works only in a DM from a sender in `SIGNAL_DM_USER_IDS`; in a group it answers that schedules are made in a DM.

```
/schedule "<message or /command>" <ISO 8601 time> [every hour|day|week|month|quarter|year] [group name]
/schedule list            (a numbered list of your schedules)
/schedule del <number>
/schedule help            (also /schedule alone)
```

- **At each run** the text is delivered (`Deliver`) from the user, as if they had posted it in the DM or the named group, with the bot posting in their place:
  - plain text is posted as written, with @Name linked to the group's members and its Markdown rendered like a reply's, and the model isn't involved;
  - text that mentions the bot is answered, like any mention;
  - a `/command` runs as a command.
- **The text** is quoted, with straight or curly quotes (phones type “ ”); the closing quote is the last one followed by a time, so the text may hold quotes. At most 2,000 characters.
- **The time** is ISO 8601, `2030-01-31T09:00` or with seconds, optionally with an offset (`Z`, `+10:00`, `+1000`, `+10`); without one it is in the bot's time zone. Years outside 2000–2999 are refused. A one-off in the past is refused; a repeating schedule that starts in the past first runs at its next time still to come.
- **Repeats** are worked out from the start, never from a late run. A daily or longer repeat keeps the start's wall-clock time across clock changes; a time the clocks skip runs as far after the change as it was written after it. A monthly schedule from the 31st runs on the last day of shorter months, and a yearly one from 29 February on 28 February in other years.
- **The group name** is the rest of the line, so it needs no quotes even with spaces. It is matched among the groups the user shares with the bot, ignoring capitals: an exact match first, otherwise with emoji and symbols removed and extra spaces collapsed on both sides ("family" matches "🏡 Family 🎉"). No match is refused with the list of the user's groups; more than one is refused as ambiguous.
- **A schedule keeps the group's ID**, so it survives a rename. If the user has left the group, the core drops that run and logs it; the schedule stays in `/schedule list`, marked as left, until deleted.
- **Numbers** are per user and never reused, so `del` of one that has already run says so rather than deleting another.
- **Missed runs:** a repeating run a minute or more late is skipped, and the next is the next time still in the future (after the due time, if the clock went back). A one-off up to an hour late runs; an older one is dropped and logged. A one-off is deleted from the saved file when it comes due.
- **No loops:** scheduling any of the four commands is refused, even after an @mention of the bot, so a schedule can't make schedules; with the bot ignoring its own posts, a scheduled mention of the bot gets exactly one answer. There is no per-user cap: only users in `SIGNAL_DM_USER_IDS` can schedule. The scheduled text is never logged; the timer's name is "schedule".
- **Refusals:** at the timer cap, or if saving fails, the schedule is refused and nothing is kept. At startup, a malformed saved schedule is logged and dropped; one that can't be set (at the timer cap) is logged and kept, listed and saved, but doesn't run until the next start. A file that can't be read stops the bot rather than being overwritten. Once a file exists, startup logs "loaded N schedules".
- **Saving:** `schedules.json` in `$STATE_DIRECTORY` (the first, if systemd lists several), or the working directory when run by hand. Each save writes a temporary file, syncs it and renames it over the old one. Schedules hold the user's ID, the group's ID and name, the time and the text until deleted. The file also keeps, for each user who has scheduled, the last number they were given (`last_id`), so numbers are never reused; it stays after their schedules are deleted (see [PRIVACY.md](PRIVACY.md)).
- **Deployment:** the systemd unit has `StateDirectory=signalbot` (`/var/lib/private/signalbot` with `DynamicUser`). The Docker image sets `STATE_DIRECTORY=/data`, bind-mounted from that same host directory. At startup `/schedule` checks that its directory can be listed and written (it creates and removes a temporary file). If not, the bot exits: for `/data`, asking for `chown 65532 /var/lib/private/signalbot`; otherwise, asking for `StateDirectory=signalbot` in the systemd unit (a unit without it runs the bot in `/`, which its dynamic user can't write) or a writable `STATE_DIRECTORY`. One install per host: the unit's dynamic user and the container's user 65532 can't share the directory.

## Tests and guardrails

For how to add a feature and the checks to run before a change, see [CONTRIBUTING.md](CONTRIBUTING.md).

Enforced by tests in `internal/arch`:

- **Imports:** only the main package, at the module root, imports features and `internal/signal`; a feature imports only `internal/feature`, `internal/kit` and its own packages; `internal/signal` imports only `internal/feature`, kit, `internal/config` (for `config.Test`) and `internal/bot` (for `Received`), so no core package imports it and no feature imports bot; `internal/feature` imports only kit; `internal/config` and kit import no other internal package, so neither imports the core.
- **No feature names in the core:** the core's source mentions no feature name or feature-only word (chart, pie, MCP, group context, web search, vision, `/intro`, `/schedule`).
- **Docs:** every setting read, the core's or a feature's (`h.Env("NAME", …)`), is in the README's configuration table.

By convention:

- **Zero features:** the core's own tests (`internal/bot`) run with no features registered, on an in-memory fake `Network`. The Signal client has its own tests against a fake signal-cli socket, and the scenarios run the real client end to end.
- **Each feature alone:** each feature's tests run on `featuretest`, a fake host.
- **Golden prompts** in `testdata` pin the system prompt byte for byte: `prompt_full.txt` with every feature registered, against real fake MCP servers rather than a hand-set list, and `prompt_nofeatures.txt`, checked by the core's own tests with no features (with tool calling on and off), and `prompt_notools.txt`, a scenario with every feature registered and a model that can't call tools. `go test . -run Golden -update` rewrites the scenarios' two files.
- **Edge-only scenarios:** `scenario_test.go` drives the whole bot, features included, through a fake signal-cli socket and a fake model API (OpenRouter's or another provider's), and checks what is sent back, what the model receives (which shows the history), and the log, including that no message text, reply, tool argument or result reaches it. [docs/SCENARIOS.md](docs/SCENARIOS.md) describes the harness and maps the tests from before the core was written to the scenarios that cover them.
- **One picture cap:** `feature.MaxPictures` (4) caps the images in a message, which may also carry a sticker, and the pictures in a reply, everywhere.
- **One test hook:** `config.Test`, nil in production, is the only test hook that changes how the core behaves: the contact reload gap, the API host treated as OpenRouter, a callback after each frame the client reads, and `Begin` and `Idle`, which count chats' workers and contact loads so a scenario can wait for the bot to be idle. The only other exports for tests read rather than change: `logx.SetOutput` and `logx.Buffer` capture the log (kit can't import config), and `config.DefaultIntro` gives the bot's tests the default `INTRO`.
- **Tests** that start slow work drain it before they end, and change the DM allowlist in place, since the client holds a reference to the map.
- **Sizes:** `go run ./internal/arch/sizes` reports lines per package; CI reports them on each PR, not enforced.

## What the bot needs from signal-cli

Only the socket. The bot speaks JSON-RPC to it and never touches signal-cli's data directory, where the account keys live.

The socket's permissions decide who may connect, and the bot is let in through a Unix group, not a user id, so a transient `DynamicUser` works:

- `signalbot.service` runs with `DynamicUser=yes` and `SupplementaryGroups=signal-cli`, so its throwaway user is in the group.
- Docker's `--group-add` puts the container's user in the same group. The container mounts only the socket's directory, so the data directory isn't there to read.
- signal-cli should create the socket group-writable (`0770`, from `UMask=0007`) in a directory the group can search (`0750`). A default umask gives `0755` and locks out even the group.

Everyone outside the Unix group gets "Permission denied". The group is the only access control: anyone in it can drive the Signal account, so keep it to the bot.

The bot's keys stay out of reach in two ways. It runs as a different user from signal-cli, and the data directory is mode `0700`, so it can't be searched. That mode matters: `UMask=0007` makes files signal-cli creates group-accessible, and the bot is in that group, so with a `0755` directory it could read them (tested). Also, `ProtectHome=yes` hides `/home`, where signal-cli keeps its data by default (`~/.local/share/signal-cli`). Data kept elsewhere, such as `/var/lib/signal-cli`, relies on the directory mode alone; add it to `InaccessiblePaths=` in the unit to hide it outright.

A TCP socket (`--tcp 127.0.0.1:7583`) needs none of this, but then any local user can connect and control or spy on the Signal account (PRIVACY RISK).
