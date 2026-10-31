# Plan: clean-slate core, features added on top (v3, from `e40dc5f`)

## Goal
- **A core written from a spec,** not carved out of the current code. It is lightweight, scales with load, and handles many chats concurrently.
- **Features bolted on cleanly,** as separate packages using a small set of ports.
- **The core never mirrors a feature,** including features long removed. Delete any feature and the core doesn't change.
- **Plain Go, no ceremony.** Use concrete types and structs freely. Add an interface only where there are two real implementations, or a test needs a fake. Define each type once and use it everywhere, rather than copying it per package. When a function needs many inputs, pass a typed struct, never a map or a long list of positional arguments. Avoid wrappers and adapters that only pass calls through.

Out of scope: chat networks other than Signal, and providers other than OpenRouter. The ports don't preclude them; they get their own plan if wanted.

## 1. Core spec: what the bot does with no features
1. **Receive:** read signal-cli's JSON-RPC socket, and reconnect with backoff if it drops. The bot's own messages are ignored.
2. **Decide if a message is addressed:** a mention of the bot, a reply to one of its messages, or a DM from an allowed sender.
   - An unaddressed group message goes to the features, and is never logged.
   - A DM from a sender not in `SIGNAL_DM_USER_IDS` is dropped. Only the sender's ID is logged.
3. **Know names,** from signal-cli's contact list:
   - loaded on connect, when a group changes, and when a mention is unknown or the list is over 24 hours old, at most every 15 seconds;
   - at most 5,000 names;
   - a group's mention names are cleared when the group changes;
   - someone with no known name is shown to the model as `[unnamed user]`;
   - the bot calls itself by its own profile name, or "signalbot" without one.
4. **Answer an addressed message:**
   - show typing until the reply is ready;
   - build the prompt: the base rules, `MODEL_SYSTEM_PROMPT`, `INTRO`, the chat's history, the message (with the sender's name in groups and any quote, escaped and capped);
   - ask the model;
   - send the reply: strip the bot's own @name, link @Name to real mentions, split at 2,000 characters, and cap the whole reply.
5. **Ask the model** through OpenRouter:
   - with `data_collection: deny`, the `MODEL_REASONING` effort, and TLS 1.3;
   - a 429 or 5xx gets one retry, and an error body is reduced to its message;
   - the cost is logged;
   - the tool loop takes at most 5 rounds and 3 minutes per reply, and 4 pictures;
   - at startup, the model's capabilities (images, tools) are read, and tools are offered only if it can call them.
6. **Remember:** a short history per chat (`HISTORY_TURNS`). Each entry is dropped after 24 hours, or sooner when the chat's disappearing-messages timer runs out. It lives in memory only. When a chat's history is gone, everything kept about that chat goes too.
7. **Run timers** (section 5).
8. **Stay safe under load:**
   - concurrency is bounded;
   - a repeated message is dropped;
   - names and quotes are made safe before they reach the model or the log;
   - message text is never logged, including in panics and error bodies;
   - all memory is capped.
9. **Configure from environment variables,** and print the version with `-version`.
10. **Log** at journald priorities 2–7, all always written: `<2>` errors that stop the bot, `<3>` errors, `<4>` warnings, `<5>` startup, connect and reconnect, `<6>` info, `<7>` debug. Under journald (`JOURNAL_STREAM` set, preferred), each line carries its `<N>` prefix. Without it, as in Docker, lines are plain, with no prefix. Either way, priorities 5–7 go to stdout and 2–4 to stderr, so a log driver that only sees the stream still tells them apart. Message content is never logged. Each addressed message logs "received group chat" or "received dm" at info; a DM from a sender not in `SIGNAL_DM_USER_IDS` always logs the sender's ID.

A message with no text (only a photo or sticker) has nothing to answer in the core. It's answered only if a feature adds something to it.

## 2. Concurrency and scale
- **Reader goroutine:** parses each frame and decides "addressed?" without allocating, locking or calling the network. Unaddressed group messages go to the `OnMessage` hooks inline (section 4).
- **Worker slots:** a fixed pool (`MAX_CONCURRENCY`). Anything that may call the network (waiting for the account ID, reloading contacts, fetching media, sticker labels) happens only after a slot is taken. When the pool is full, new work is dropped with one warning.
- **Per-chat mailbox:** an addressed message, or one a feature delivers (section 4), joins its chat's queue. At most 4 can wait; more are dropped with a warning. A chat with work has one worker, holding one slot, which answers the queue in order and then exits, so each answer sees the ones before it. Different chats run in parallel. *This fixes today's behaviour: two messages in one chat are answered at the same time, so the second can't see the first, and history is stored in reply order.*
- **Per-chat state:** one struct per chat, in one capped map: the history and its expiry timer, recent message keys (for dedupe), mention names, the tool groups' follow-up counts, and the mailbox. Its lock is held only to read or change it, never across a model call. When the history is gone, the struct goes.
- **HTTP:** the model client keeps up to `MAX_CONCURRENCY` idle connections (Go's default is 2 per host).
- **Shutdown:** timers stop, new work stops, slots in flight drain (up to 8s), then the socket closes.

## 3. Architecture
```
cmd/signalbot/          main.go (wiring), features.go (one line per feature)
internal/signal/        the signal-cli client, names and mentions
internal/openrouter/    the model client and the tool loop
internal/chats/         per-chat state: history, dedupe, mention names, the mailbox
internal/timers/        one-shot timers
internal/config/        settings from the environment, each name recorded for the docs test
internal/bot/           the decision, the prompt, the hooks; wires the packages above
internal/feature/       the ports and shared types (the only part of the core features import), the command helper, featuretest
internal/features/*     one package per feature
internal/kit/*          code with two or more users and no bot knowledge
```
- Core packages are concrete and call each other directly, with no interfaces between them. Files by role, no line limit.
- `Message`, `Media`, `Picture`, `Turn` and the port types are defined once, in `internal/feature`, and the core uses them directly: nothing is copied or converted.
- Kit starts with what both the core and features use: `logx`, `txt`, `img` and `httpx`. It gains a package only when a second user appears.

## 4. Lifecycle and ports
The core's lifecycle has four stages. Each port is one stage; nothing else is a port.

| Stage | Port | Returns |
|---|---|---|
| A message arrives | `OnMessage(func(ctx, Message) Action)` | carry on, ignore it, answer with text without the model, choose a tool, or keep text as context |
| The model is asked | `OnTurn(func(ctx, Turn) Additions)`, plus `Tools(ToolGroup)` | a system-prompt line, a note on the message (with its own version for history), and images |
| The reply goes out | `OnReply(func(Turn, *Reply))` | may change the text or pictures |
| Time arrives | `At` (section 5) | nothing: the timer may deliver a message |

**Hook rules:**
- Hooks run in feature order (`features.go`).
- For an unaddressed message, `OnMessage` runs on the reader goroutine, so it must not block or call the network. For an addressed one it runs in the message's slot. The first non-zero `Action` wins.
- `Answer` and `ToolChoice` count only on an addressed message, and `Context` only on an unaddressed one.
- `OnReply` runs only after the model replies.

**Services the host offers:**
- `Env`: a feature's own settings, recorded for the docs test.
- `Info`: the bot's name in a chat, the model, whether it takes images and can call tools, and the names of the tools.
- `Media(ctx, msg, i)`: the bytes of a message's attachment. The core fills in each item's label (a sticker's emoji and pack) when it loads the message.
- `Deliver(Message)`: handle a message as if its sender had just sent it to the bot (commands, hooks and the model), through its chat's mailbox.
- `Groups(ctx, user)`: the groups the bot shares with a user, with their names (signal-cli's `listGroups`).

**Types**, each field being a stage's natural input or output:
```go
type Action    struct{ Ignore bool; Answer, ToolChoice, Context string } // zero value: carry on
type Additions struct{ System, Note, HistoryNote string; Images []string }
type Reply     struct{ Text string; Pictures []Picture; ToolsUsed []string }
type Picture   struct{ Data []byte; Caption, HistoryNote string }
type ToolGroup struct{ Name, Listing, System string; Tools []Tool; Keywords *regexp.Regexp }
```
- **`ToolChoice`** is the model API's own concept, not a chat-command field.
- **`Context`** is "keep this text in history", which the core does under its own expiry rules.
- **`ToolsUsed`** is what the tool loop reports.
- **A picture's `HistoryNote`** is set by the tool that made it; without one, the core records "[sent an image]". A reply with no text is sent with its pictures' captions, one per line.
- **`ToolGroup`:** `Name` is how the intro's `{tools}` lists it. `Listing` is its item in the tools line, which the core ends with "If asked what tools you have:" and every group's `Name`, offered or not. `System` is a system-prompt line while the group is offered.
- **Offering:** a group without `Keywords` is always offered. One with `Keywords` is offered when a message matches, and for the 2 replies after any reply that matched or used one of its tools. A `ToolChoice` offers only that tool.
- **`Turn`** is the message being answered, in its chat.

**Commands** are a helper in `internal/feature` built on `OnMessage`. A name taken by another feature is refused at registration. The core doesn't list commands: a feature names its own in its tool group's listing.

## 5. Timers (core)
Time is a source of work, like a message, so timers belong in the core. The `/schedule` command, reminders and digests are features on top of them.
```go
At(name string, t time.Time, fn func(ctx context.Context)) (cancel func())
```
- **One-shot:** a timer runs once and is removed. A feature that repeats sets its next timer when one runs, so the core needs no cron.
- **One goroutine** keeps every timer in a min-heap and wakes only when one is due, so idle costs nothing.
- **Each run takes a worker slot,** like a message. If none is free, the run is skipped with one warning.
- **Caps:** the core caps the total number of timers.
- **Time zone:** the local one. Go reads `TZ` itself (UTC in the container), and the distroless image ships tzdata, so no code is needed.
- **Logs:** the timer's name and how long the run took. Errors are logged.
- **Memory only:** timers are lost on restart, and a run missed while the bot was down isn't replayed. A feature that saves its own (`/schedule`) sets them again at startup.

## 6. Features, added one at a time on the ports
| Feature | Ports | Owns |
|---|---|---|
| `example` | command helper | `/example` |
| `websearch` | `Tools` | OpenRouter's web search tool, `MODEL_SEARCH`, the "web search on" log line |
| `groupctx` | `OnMessage` returning `Context` | `GROUP_CONTEXT`, the `[image]`/`[sticker]` notes |
| `vision` | `OnTurn`, `Info`, `Media` | answering photo- or sticker-only messages, the image and sticker notes (using the labels the core loads), the "images aren't kept" history note |
| `mcp` | `Tools` (one group per server), command helper returning `ToolChoice` | `MCP_CONFIG`, servers, `chat_commands`, and per server an optional `prompt` (else the server's own `instructions`, capped) and `keywords` |
| `charts` | `Tools` with `Keywords`, `OnReply` | the chart tools and their keywords, each chart's caption and history note, dropping the model's text when a chart is sent |
| `schedule` | command helper, `At`, `Deliver`, `Groups` | `/schedule` (below) |

In `features.go`, `websearch`, `charts` and `mcp` stay in that order: it's the order of the tools line and `{tools}`.

**`/schedule`** lets a user schedule anything they could type to the bot. It works only in a DM from a sender in `SIGNAL_DM_USER_IDS`.
```
/schedule "<message or /command>" <ISO 8601 time> [every hour|day|week|month|quarter|year] [group name]
/schedule list            (a numbered list of your schedules)
/schedule del <number>
/schedule help
```
- At each run, the text is delivered as if the user had sent it, in the DM or the named group: commands, hooks and the model all apply.
- The group must be one the user shares with the bot, checked again at each run.
- A time without an offset is in the bot's time zone. A monthly schedule from the 31st runs on the last day of shorter months.
- Straight and curly quotes both work (phones type “ ”).
- Each user has a cap, scheduling `/schedule` itself is refused, and the text is never logged.
- Schedules are always saved as JSON (standard library, no new dependencies) in `$STATE_DIRECTORY`, or the working directory when run by hand. Each save writes a temporary file and renames it over the old one.
- The systemd unit gets `StateDirectory=signalbot` (`/var/lib/private/signalbot` with `DynamicUser`). The Docker image sets `STATE_DIRECTORY=/data`, and the README bind-mounts that same host directory there. If `/data` isn't readable and writable, the bot exits with a message asking for `chown 65532 /var/lib/private/signalbot`. One install per host: the unit's dynamic user and the container's user 65532 can't share the directory.

**If a feature can't be built on these ports, that's a design finding.** Either the port becomes more generic (and stays tied to its stage), or the feature takes on more of the work. A feature-shaped field is never the answer.

## 7. Fixes from the first recode, carried over
- **Golden prompts:** one with every feature registered and one with none, pinned before work starts. The rebuilt bot matches them byte for byte, apart from changes approved in section 10. The fixture uses real fake MCP servers, not a hand-set list.
- **Behaviour scenarios:** today's tests become scenarios that touch only the edges: messages in on a fake signal-cli socket, then what's sent back, what a fake OpenRouter receives (which shows the history), and the logs. A small start function per version wires up the fakes. The scenarios and assertions pass before and after, changed only where section 10 approves. A table in the PR maps every old test to a new one.
- Message handling stays two-phase: a cheap decision inline, and network calls only after a slot is taken.
- The names used to link @Name mentions are forgotten with the chat's history.
- A command name is checked at registration, not against a hard-coded list.
- One constant, in `internal/feature`, caps the pictures in a message (4).
- When code moves between files, check that no line is lost (the first recode silently dropped two multi-line constants).
- Kit never imports the core (which caused an import cycle).
- Tests that start slow work drain it before they end, or their log lines land in the next test.
- Tests change the DM allowlist in place, because the client holds a reference to the map.
- Docker: the `.dockerignore` and `COPY` include the packages, the build targets `./cmd/signalbot`, and the README's build step matches.

## 8. Guardrails (Go tests)
- **Imports:** only `cmd/signalbot` imports features; features import only `internal/feature`, `internal/kit` and their own packages; kit imports no other internal package.
- **No feature names in the core:** the core's source mentions no feature name or feature-only word (chart, MCP, group context, `/example`, `/schedule`).
- **Zero features:** the core's own tests run with no features registered.
- **Each feature alone:** each feature's tests run on `featuretest`, a fake host.
- **Docs:** every setting read is in the README's configuration table.
- **Sizes:** reported on each PR, not enforced.

## 9. Steps
0. **Reset:** record the branch's head SHA, reset the branch to `e40dc5f`, and re-add this plan, CI and the sizes report as the first commits. Ask before force-pushing.
1. **Pin behaviour at `e40dc5f`:** the scenarios and both golden prompts (section 7).
2. **Write the core from section 1**, reusing code (the signal client, names, the decision rules, prompt building, the OpenRouter client, the tool loop, history) but not its structure. Add the ports, timers and guardrails. Acceptance: it runs and passes its tests with zero features, and matches the no-features golden prompt. **Size checkpoint:** report the core's lines before going on.
3. **Add the features one at a time** in section 6's order. Each moves its logic and tests into its package, and adds its line to `features.go`. After `charts`, the full golden prompt and the scenarios pass.
4. Add the `schedule` feature with its tests.
5. Docs:
   - TECHNICAL.md: the core spec, then one paragraph per feature;
   - CONTRIBUTING.md: the port rule and the guardrails;
   - README: the settings tables, `TZ`, `/schedule`, and the MCP `prompt` and `keywords` fields;
   - PRIVACY.md: scheduled text is kept on disk until deleted.

## 10. Behaviour changes
**Approved:**
- Messages in one chat are answered one at a time, in order, with at most 4 waiting (section 2).
- The tools line lists each MCP server as its own item, without "MCP tools by server". Today it ends "name them, grouped by server" when MCP tools exist; see the full list below.
- MCP's `prompt` and `keywords` are new and optional: without them, nothing changes.
- `/schedule` is new, and `TZ` sets its time zone. Schedules are always saved, so they survive a restart (section 6).
- The donut chart is renamed pie: the tool is `make_pie_chart`, still drawn as a ring with the total in the middle. Its description still mentions donuts.
- Settings: `REASONING` becomes `MODEL_REASONING`, `SYSTEM_PROMPT` becomes `MODEL_SYSTEM_PROMPT`, and `LOG_LEVEL` is dropped.
- Logging (section 1): journald priorities 2–7, always written, with the `<N>` prefix only under journald; 5–7 to stdout and 2–4 to stderr. Each addressed message is logged at info as "received group chat" or "received dm", with no name or content.
- The websearch feature logs "web search on", and the startup line drops `search …`. The "OpenRouter error" line keeps today's wording.
- When the model can't call tools, the core's warning says "tools are off" in place of naming web search, charts and MCP.

- The tools line ends with the full list of tools, not only those offered this turn: "If asked what tools you have: web search, charts, …". So "what tools do you have?" names charts and keyword-gated MCP servers too.

## 11. Expected size
About +650 to +850 lines over 4,185, checked after the core (step 2):
- one-shot timers, about 80;
- the ports, the registry, the per-chat mailbox and the offering rule, about 220;
- a package per feature;
- the `schedule` feature, about 250: parsing, list, delete, help, repeats and saving;
- MCP's `prompt` and `keywords`, about 30.
