# Plan: clean-slate core, features added on top (v3, from `e40dc5f`)

## Goal
- **A core written from a spec,** not carved out of the current code. It is lightweight, scales with load, and handles many chats concurrently.
- **Features bolted on cleanly,** as separate packages using a small set of ports.
- **The core never mirrors a feature,** including features long removed. Delete any feature and the core doesn't change.

Out of scope: chat networks other than Signal, and providers other than OpenRouter. The ports don't preclude them; they get their own plan if wanted.

## 1. Core spec: what the bot does with no features
1. **Receive:** read signal-cli's JSON-RPC socket, and reconnect with backoff if it drops.
2. **Decide if a message is addressed:** a mention of the bot, a reply to one of its messages, or a DM from an allowed sender. Everything else is unaddressed. It's handed to the features and is never logged.
3. **Answer an addressed message:**
   - build the prompt: the base rules, `SYSTEM_PROMPT`, `INTRO`, the chat's history, the message (with the sender's name in groups and any quote, escaped and capped);
   - ask the model;
   - send the reply: strip the bot's own @name, link @Name to real mentions, split at 2,000 characters, and cap the whole reply.
4. **Remember:** a short history per chat (`HISTORY_TURNS`). Each entry is dropped after 24 hours, or sooner when the chat's disappearing-messages timer runs out. It lives in memory only. When a chat's history is gone, everything kept about that chat goes too.
5. **Run scheduled jobs** (section 4).
6. **Stay safe under load:**
   - concurrency is bounded;
   - a repeated message is dropped;
   - names and quotes are made safe before they reach the model or the log;
   - message text is never logged, including in panics and error bodies;
   - all memory is capped.
7. **Configure from environment variables,** and log at four levels (journald priorities under systemd).

A message with no text (only a photo or sticker) has nothing to answer in the core. It's answered only if a feature adds something to it.

## 2. Concurrency and scale
- **Reader goroutine:** parses each frame and decides "addressed?" without allocating, locking or calling the network. Unaddressed messages go to the `OnMessage` hooks inline. Those hooks must be quick, and may not block or call the network.
- **Worker slots:** a fixed pool (`MAX_CONCURRENCY`). An addressed message, or a scheduled job, takes a slot before any goroutine starts; when the pool is full, it is dropped with one warning. Anything that may call the network (waiting for the account ID, reloading contacts, fetching media) happens only after the slot is taken.
- **Chat order:** messages in the same chat are answered one at a time, in order; different chats run in parallel. Each chat has its own lock, created on demand and dropped with the chat's history. *This is a change: today, two messages in one chat are answered at the same time and their history can interleave.*
- **Memory:** state lives in maps keyed by chat ID, each capped, so memory is bounded under load.
- **Shutdown:** new work stops, slots in flight drain (up to 8s), then the socket closes.

## 3. Architecture
```
cmd/signalbot/          main.go (wiring), features.go (one line per feature)
internal/bot/           the core: signal client, names/mentions, decide, prompt, OpenRouter client and tool loop,
                        history, scheduler, registry, config, logging
internal/feature/       the ports (the only part of the core features import), the command helper, featuretest
internal/features/*     one package per feature
internal/kit/*          code with two or more users and no bot knowledge
```
- The core is one package with files by role and no line limit.
- `internal/feature` holds types and helpers only.
- Kit starts empty and gains a package only when a second user appears.

## 4. Lifecycle and ports
The core's lifecycle has five stages. Each port is one stage; nothing else is a port.

| Stage | Port | Returns |
|---|---|---|
| A message arrives (addressed or not) | `OnMessage(func(ctx, Message) Action)` | carry on, answer with text without the model, keep text as context in history, or choose a tool |
| The model is asked | `OnTurn(func(ctx, Turn) Additions)`, plus `Tools(ToolGroup)` with `Offer(Turn) bool` | a system-prompt line, a note on the message (with its own version for history), and images |
| The reply goes out | `OnReply(func(Turn, *Reply))` | may change the text, pictures or history note |
| A chat is forgotten | `OnForget(func(chat))` | nothing: drop state about the chat |
| Time arrives | `Schedule(name, every, func(ctx, Job))` | nothing: the job may post or ask (below) |

**Services the host offers:**
- `Env`: a feature's own settings, recorded for the docs test.
- `Info`: the bot's name in a chat, the model, the names of the tools.
- `Media(ctx, msg, i)`: the bytes of a message's attachment.

**Types**, each field being a stage's natural input or output:
```go
type Action    struct{ Answer, Context, ToolChoice string }              // zero value: carry on
type Additions struct{ System, Note, HistoryNote string; Images []string }
type Reply     struct{ Text, HistoryNote string; Pictures []Picture; ToolsUsed []string }
type Job       interface{ Post(chat, text string, pics ...Picture) error; Ask(ctx, chat, prompt string) error }
```
- **`ToolChoice`** is the model API's own concept, not a chat-command field.
- **`ToolsUsed`** is what the tool loop reports.
- **`Context`** is "keep this text in history", which the core does under its own expiry rules.

**Commands** are a helper in `internal/feature` built on `OnMessage`. A name taken by another feature is refused at registration.

## 5. The scheduler (core)
Time is a source of work, like a message, so the scheduler belongs in the core. Reports, reminders and digests are features on top of it.
- **`Schedule(name, every, fn)`** takes a 5-field cron expression or a Go duration such as `1h`, read from the feature's settings.
- Times are in `TZ` (default UTC). Runs are evaluated by one timer goroutine over a min-heap of next run times. It wakes only when something is due, so idle costs nothing.
- **Each run takes a worker slot.** A run still going when the next is due is skipped, with one warning. A missed run (because the bot was down) isn't replayed: schedules are kept in memory, from features' settings, like everything else.
- **`Job.Post`** sends text and pictures to a chat as the bot.
- **`Job.Ask`** runs a full turn: the same prompt, hooks and tools as a message, in that chat, with the history entry recorded as a scheduled prompt.
- **Logs:** the job's name and duration, never what it posted.
- **Errors** are logged, and the next run proceeds.
- **The first user** is an example scheduled report: for example, `REPORT_CHAT`, `REPORT_EVERY` and `REPORT_PROMPT` for a daily summary. It's built as a feature to prove the port, and the README notes it as a template.

## 6. Features, added one at a time on the ports
| Feature | Ports | Owns |
|---|---|---|
| `example` | command helper | `/example` |
| `websearch` | `Tools` | OpenRouter's web search tool, `MODEL_SEARCH` |
| `groupctx` | `OnMessage` returning `Context` | `GROUP_CONTEXT`, the `[image]`/`[sticker]` notes |
| `vision` | `OnTurn`, `Media` | answering photo- or sticker-only messages, image and sticker notes, sticker labels, the "images aren't kept" history note |
| `mcp` | `Tools`, command helper returning `ToolChoice` | `MCP_CONFIG`, servers, `chat_commands` |
| `charts` | `Tools` with `Offer`, `OnReply`, `OnForget` | the chart tools, the 2-reply follow-ups, the one-message caption and its history note |
| `report` | `Schedule`, `Job.Ask` | the template scheduled report |

**If a feature can't be built on these ports, that's a design finding.** Either the port becomes more generic (and stays tied to its stage), or the feature takes on more of the work. A feature-shaped field is never the answer.

## 7. Fixes from the first recode, carried over
- A golden test of the full system prompt with every feature registered, pinned before work starts. The rebuilt bot must match it byte for byte. A second golden test pins the no-features prompt.
- Message handling stays two-phase: a cheap decision inline, and network calls only after a slot is taken.
- The names used to link @Name mentions are forgotten with the chat's history.
- A command name is checked at registration, not against a hard-coded list.
- When code moves between files, check that no line is lost (the first recode silently dropped two multi-line constants).
- Kit never imports the core (which caused an import cycle).
- Tests that start slow work drain it before they end, or their log lines land in the next test.
- Tests change the DM allowlist in place, because the client holds a reference to the map.
- Docker: the `.dockerignore` and `COPY` include the packages, the build targets `./cmd/signalbot`, and the README's build step matches.

## 8. Guardrails (Go tests)
- **Imports:** only `cmd/signalbot` imports features; features import only `internal/feature`, `internal/kit` and their own packages; kit imports no other internal package.
- **No feature names in the core:** the core's source mentions no feature name or feature-only word (chart, donut, MCP, group context, `/example`, report).
- **Zero features:** the core's own tests run with no features registered.
- **Each feature alone:** each feature's tests run on `featuretest`, a fake host.
- **Docs:** every setting read is in the README's configuration table.
- **Sizes:** reported on each PR, not enforced.

## 9. Steps
1. Pin the two golden prompts and the integration tests against `e40dc5f`.
2. **Write the core from section 1**, reusing code (the signal client, names, the decision rules, prompt building, the OpenRouter client, the tool loop, history) but not its structure. Add the ports, the scheduler and the guardrails. Acceptance: it runs and passes its tests with zero features, and matches the no-features golden prompt.
3. **Add the features one at a time** in section 6's order. Each moves its logic and tests into its package, and adds its line to `features.go`. After `charts`, the full golden prompt and the integration tests pass unchanged.
4. Add the `report` feature with its tests.
5. Docs:
   - TECHNICAL.md: the core spec, then one paragraph per feature;
   - CONTRIBUTING.md: the port rule and the guardrails;
   - README: the settings tables, and `report` as the template for scheduled features.

## 10. Behaviour changes, asked about before building
- Messages in one chat are answered one at a time (section 2).
- The scheduler and the `report` feature are new.
- New settings: `TZ`, and the report's settings.

## 11. Expected size
About +500 to +700 lines over 4,185:
- the scheduler, about 150;
- the ports and the registry, about 150;
- a package per feature;
- the report feature, about 60.
