# Plan: modular recode — a neutral core, adapters for networks and models, features behind ports

## Context
signalbot is one flat `package main` of about 4,000 lines of code and 3,700 of tests. Two kinds of leak make it hard to change:
- **Features in the core.** `bot.go` (about 1,000 lines) holds the chart follow-up counter, chart captions, image and sticker fetching, group chatter, `/example` and the system-prompt tool line. `openrouter.go` dispatches chart tools by name. `main.go` wires MCP straight into the model client.
- **Signal and OpenRouter in the core.** Message decisions work on Signal envelopes, names come from Signal contacts, the prompt says "Signal chat", and the model client is OpenRouter's.

The goal is a core that knows no feature, no chat network and no model provider. Features live in their own folders behind small ports. Signal (and later Telegram, possibly at the same time) and the model provider are adapters. **Behaviour doesn't change:** tests move with their code and pass at every step. Anything that would change behaviour is asked about first (see "Simplifications that change behaviour" below).

This merges the original plan with the later design changes: networks and providers as adapters, several networks at once, a switchable provider, and the shared message splitter. It also records the work already done.

## Rules
1. **The core knows no feature, network or provider.** Only `cmd/signalbot` imports features and adapters. `cmd/signalbot/features.go` lists the features, one line each.
2. **Ports only.** A feature reaches the world through the `feature.Host` it is given. It may import `feature`, the neutral `chat` types and kit, but never core internals or another feature.
3. **A feature owns everything it needs:** settings, triggers, commands, tools, prompt lines, a package doc comment describing it, and tests, all in `internal/features/<name>/`.
4. **Shared pure code is a library** in `internal/kit/*`. Anyone may import it, and it imports nothing internal but other kit.
5. **Data-source adapters** go in `internal/sources/<server>` once a feature depends on one MCP server's output shape. None does yet, so the package isn't created.
6. **Small files, reported sizes.** One role per file, aiming for ~200 lines; a file over 300 fails a test, tests included. A package over 1,500 lines must split. Sizes are reported on every PR.

## Layout
```
cmd/signalbot/                main.go (wiring), features.go (the feature list)
internal/chat/                neutral types: Message, Outgoing, Media, ChatID, the Transport interface
internal/bot/                 routing, dedupe, concurrency, respond loop, prompt base, commands mechanism, shutdown
internal/llm/                 Model interface, capabilities, the provider-free tool loop
internal/llm/openai/          OpenAI-compatible client, with OpenRouter's extras when pointed at it
internal/history/             per-chat memory and expiry
internal/config/              core settings
internal/feature/             the ports
internal/transports/signal/   the Signal adapter (+ signaltest/)
internal/features/            example, intro, websearch, mcp, groupctx, vision, charts
internal/kit/                 logx, txt, img, draw, linechart, donut, mcp
internal/arch/                guardrail tests and the sizes tool (utilities)
```

## Core
The test for core: **with no features registered and a fake transport, the bot answers an addressed message with a model reply, remembers the conversation, and forgets it on time.**

| Package | Holds |
|---|---|
| `chat` | `Message`, `Outgoing`, `Media`, `ChatID` and `Transport` (below) |
| `bot` | <ul><li>Routes each `Message`: addressed ones are answered, unaddressed ones go to the `OnUnaddressed` hooks</li><li>Dedupe (chat, sender, text, media IDs), concurrency, typing, the respond loop</li><li>The reply policy: whole-reply cap, removing the bot's own @name, truncation marker</li><li>The neutral prompt base, with the transport's `Describe()` spliced in</li><li>Command dispatch as a mechanism, and shutdown</li></ul> |
| `llm` | The `Model` interface (one request: messages, tools, forced tool), capabilities, and the tool loop (5 rounds, 3-minute budget, 30s per tool, result caps). It runs whatever tools are registered |
| `history` | Per-chat memory: `HISTORY_TURNS`, 24 hours, the message's disappearing timer, one eraser per chat. Erasing a chat calls `OnForget` hooks and the owning transport's `Forget`. A privacy promise, so it's tested centrally |
| `config` | Core settings: model, reasoning, history, concurrency, log level, extra prompt |
| `feature` | The ports |

### The neutral chat model (`internal/chat`)
```go
type ChatID string // "<transport>:<kind>:<id>", e.g. "signal:g:…", "telegram:c:…"

type Message struct {
    Chat      ChatID
    Group     bool
    Sender    Person        // ID, Name (already made safe)
    Text      string        // mentions shown as "@Name"; the bot's own removed
    Addressed bool          // mention of the bot, reply to it, or an allowed DM
    Quote     *Quote        // Text, Author, FromBot
    Media     []Media       // ID, Kind (image|sticker), Label (e.g. sticker emoji and pack)
    Expires   time.Duration // disappearing timer, 0 if none
}
type Outgoing struct{ Chat ChatID; Text string; Images []Image } // Image: data + caption

type Transport interface {
    Kind() string                                  // "signal": the ChatID prefix
    Run(ctx context.Context, handle func(Incoming)) // reconnects itself; delivers unaddressed group messages too
    Send(ctx context.Context, m Outgoing) error    // splits at its own limit, turns known "@Name" into mentions
    Typing(chat ChatID)
    Fetch(ctx context.Context, chat ChatID, m Media) ([]byte, error)
    Name() string     // the bot's display name on this network
    Describe() string // prompt text: network name, markdown or not, message limit
    Forget(chat ChatID) // drop per-chat state (Signal: known names for mentions) when the core erases the chat
}
```

**`Incoming` and `Load`.** A transport hands over an `Incoming`: chat, group, and whether the bot is addressed, judged cheaply on the reader goroutine. It also carries `Load`, which builds the full `Message`. The core calls `Load` only after taking a concurrency slot, because building the message may call the network: waiting for the account ID, reloading contacts for an unknown mention, fetching sticker labels. This keeps today's rule that ignored messages cost nothing and no sender can cause unbounded work.

**What a transport delivers.** Addressed messages, and unaddressed messages from groups only. A DM from a sender who isn't allowed is dropped inside the transport and never reaches the core. Every adapter has a test for this privacy rule, matching today's behaviour.

### Several networks at once (~30 lines)
- `cmd/signalbot` builds every network whose settings are present: `SIGNALCLI_SOCKET` for Signal, a token for a future Telegram adapter. There's no list setting, and startup fails clearly if none is configured.
- `bot` runs each `Run` with the same handler and one concurrency limit. A router sends `Send`, `Typing` and `Fetch` to the transport whose `Kind()` prefixes the chat ID.
- History, dedupe and per-chat feature state are keyed by chat ID, so networks stay apart without extra code. Chats are never bridged.

### One model provider, switchable
- One OpenAI-compatible adapter, `llm/openai`. When it points at OpenRouter (the default), it turns on the extras:
  - `data_collection: deny`;
  - cost in usage;
  - the web search server tool;
  - the models list for capabilities.
- `MODEL_API_URL` (default OpenRouter) and `MODEL_API_KEY` choose the provider. `OPENROUTER_API_KEY` keeps working, so existing setups don't change.
- OpenRouter's models list supplies capabilities. Settings for providers without one (`MODEL_VISION`, `MODEL_TOOLS`) are deferred until such a provider is first used.
- Several providers at once, and native Anthropic or Gemini clients, are left out. The `Model` interface leaves room for them.

### Message splitting
- **The algorithm:** `kit/txt.Split(text, limit)`, today's `splitMessage`, moved as is.
- **The policy:** the whole-reply cap and the self-tag stripping stay in `bot`.
- **The limit:** each transport calls `Split` in `Send` with its own limit, 2,000 for Signal. Mention offsets are counted per part, after the split.

## The Signal adapter (`internal/transports/signal`)
It holds everything Signal-specific:
- the signal-cli client (done, now at `internal/signal`; it moves here);
- names and contacts (`names.go`);
- envelope → `chat.Message`: today's `trigger`, `Decide`, `stripMentions`, sender and allowlist checks, quotes, and the sticker label from `listStickerPacks`;
- the per-chat known names, so "@Name" in a reply becomes a real mention (`mentions.go`, `remember`/`knownNames`), dropped in `Forget`;
- its 2,000-character limit, typing, and media fetch;
- its `Describe()`: "a Signal chat … plain text: Signal does not render markdown … 2000 characters";
- its settings: `SIGNALCLI_SOCKET`, `SIGNAL_DM_USER_IDS`.

It's tested from envelope fixtures to `chat.Message`, and the core tests never build an envelope. A Telegram adapter later is about 400–600 lines with no new dependencies; it isn't part of this plan.

## Features (`internal/features/<name>`)
- **`example`**: `/example`.
- **`intro`**: the introduction prompt line and `INTRO`, with `{tools}` from the port.
- **`websearch`**: the OpenRouter server tool, registered as a provider tool, and `MODEL_SEARCH`. It is offered only when `Caps` says the provider supports server tools.
- **`mcp`**:
  - `MCP_CONFIG`: starts the servers, registers their tools, and adds the "MCP tools by server" prompt text;
  - `chat_commands`: `/name` runs a forced-tool turn, and `require_arg` gets a bare-command reply from the bot;
  - its startup checks.
- **`groupctx`**: `GROUP_CONTEXT`. It keeps unaddressed group messages in history through `Remember`.
- **`vision`**: fetches images and stickers through `Media().Fetch` when the model can see images. It uses the transport's media label, and writes the "images aren't kept" history marker.
- **`charts`**, one package with a file per role, about 500 lines once drawing is in kit:
  - `line.go` and `donut.go`: tool definitions, argument decoding and errors, summaries;
  - `offer.go`: trigger words and 2 follow-ups, cleared in `OnForget`;
  - `caption.go`: the one-message captioned reply, the one-line title-and-subtitle caption, and the "[sent the chart …]" note.

**Kit:**
- `logx` (done) and `txt` (done: `Safe`, `Clamp`; `Split` arrives with the reply code);
- `img`: the image checks, data URIs, and `MaxImageBytes`, which moves here from `signal`;
- `draw`, `linechart`, `donut`;
- `mcp`: the MCP protocol client, with no bot knowledge.

## Ports (`internal/feature`)
Hooks return what they add, instead of changing what they're handed. That makes each one testable as a plain function, and lets the core decide the order and apply the results.
```go
type Feature interface{ Name() string; Register(h Host) error }

type Host interface {
    Env() Env                                       // Bool/Int/String with defaults; names recorded for the docs test
    Caps() Caps                                     // vision, tools, server tools
    Info() Info                                     // model name, offered tool names (/example, intro)
    Media() Media                                   // Fetch(chat, media)
    Command(name string, run func(context.Context, chat.Message) Command) // Command{Text} answered by the bot, or Command{Tool} for a forced-tool turn
    Tool(t Tool)                                    // def, Offer(Turn) bool, Run(ctx, args) Result{Text, Images with captions}; or a provider tool (raw entry, no Run), e.g. web search
    OnTurn(func(context.Context, Turn) Additions)   // Additions{Prompt, ModelNote, HistoryNote, Images}: intro, mcp, vision
    OnReply(func(Turn, Completion) *chat.Outgoing)  // a replacement reply, or nil: charts captions; also sees what was used (follow-ups)
    OnUnaddressed(func(chat.Message))               // groupctx
    OnForget(func(chat.ChatID))                     // per-chat state goes with the history
    Remember(chat chat.ChatID, text string)         // groupctx: into history under the core's expiry
}
```
- `Turn` is read-only: the message, its chat, the bot's name on that network, and the tools offered.
- **`featuretest`:** a fake `Host` for testing each feature alone. It records registrations, runs hooks, and provides a fake media fetch.
- Logging goes through `kit/logx`.
- The core builds "Your tools: …" from the tools offered that turn, so it names none itself.

## Guardrails (Go tests; done in step 1, tightened as packages appear)
- **Imports:**
  - the core doesn't import `features`, `transports` or `llm/openai`;
  - kit imports only kit;
  - features import only `feature`, `chat` and kit;
  - no feature imports another.
- **Sizes:** a file over 300 lines or a package over 1,500 fails. The files that were already oversized are listed, and an entry goes when its file is split; none may be added.
- **Isolation:** the core tests run with no features and a fake transport, and each feature's tests run with only that feature, through `featuretest`.
- **Golden prompt:** a test of today's full system prompt for the Signal setup, with all features. It's written before the split (the first commit of step 2's next part), and the assembled prompt must stay byte-identical.
- **Docs:** every setting read through `Env()` must be in the README config table.
- **CI:** gofmt, vet, `test -race`, and the sizes table with its change from the base branch in the job summary.

## Steps (behaviour unchanged at every step)
1. ~~Guardrails and CI~~: `3672fd9`.
2. **Packages.**
   - Done:
     - `kit/logx` (`541178d`);
     - Docker builds from the packages (`f6cfc57`);
     - the signal-cli client and `kit/txt` (`c547f58`).
   - Next:
     1. ~~the golden prompt test against today's code; `internal/chat`, and the client moved to `internal/transports/signal`~~ (done);
     2. ~~the adapter takes over names, message decisions, known names and mentions, emits `chat.Message`, and gets `Describe()`. `bot` works only on `chat` types, with the router~~ (done; `kit/txt` gained `Split` and `AtNames`, and `kit/bound` holds the bounded-map helper);
     3. ~~`history` out of `bot`; `kit/txt.Split`~~ (done);
     4. ~~`llm` with the tool loop, and `llm/openai`. `MODEL_API_URL` and `MODEL_API_KEY` are new settings, added with it~~ (done; the TLS 1.3 client is `kit/httpx`);
     5. main moves to `cmd/signalbot`, and Docker builds `./cmd/signalbot`. Moved to step 4: it happens when the bot core becomes `internal/bot`.
3. ~~**Kit:** `img`, `draw`, `linechart`, `donut`, `mcp`~~ (done; also `bound`, `httpx`, `txt`).
4. **Ports and registry.** The core calls the hooks; tools and prompts come from the registry.
5. **Features out**, deleting the core's copy each time, in this order:
   1. `example`
   2. `intro`
   3. `websearch`
   4. `mcp`
   5. `groupctx`
   6. `vision`
   7. `charts`
6. **Docs:**
   - TECHNICAL.md covers the core and adapters, with a paragraph per feature linking to its package, whose doc comment describes it;
   - CONTRIBUTING.md states the rules;
   - the README config table is grouped by owner.

The work goes in one PR per step, or "pr and merge" whenever asked.

## Configuration
- **Shared settings are unchanged** and apply to every network: `MODEL`, `REASONING`, `HISTORY_TURNS`, `MAX_CONCURRENCY`, `SYSTEM_PROMPT`, `INTRO`, `GROUP_CONTEXT`, `MCP_CONFIG`, `MODEL_SEARCH` and `LOG_LEVEL`.
- **Each network has its own prefixed settings, and they also switch it on:**
  - Signal: `SIGNALCLI_SOCKET` and `SIGNAL_DM_USER_IDS`, unchanged;
  - a future Telegram adapter: `TELEGRAM_BOT_TOKEN` and `TELEGRAM_DM_USER_IDS`.

  Allowlists stay per network, because user IDs differ.
- **One check changes:** "SIGNALCLI_SOCKET is required" becomes "configure at least one network (SIGNALCLI_SOCKET, …)". Existing setups see no difference.
- **Provider:** `MODEL_API_URL` (default OpenRouter) and `MODEL_API_KEY`. `OPENROUTER_API_KEY` is still accepted.
- **Left out on purpose:** per-network intros, models or prompts, until someone asks for them.
- **Docs:** the README config table is grouped as Core / Model / Signal / Features. The systemd unit's `signal-cli` group membership is marked Signal-only.
- **The name stays signalbot** (binary, service, image). Renaming would touch every deployment for no functional gain.

## Tests
- **About 3,700 lines of tests move with the code they test:**
  - the bot's tests go to `bot`, on a fake transport;
  - the envelope tests go to the Signal adapter;
  - the chart tests go to `charts` and kit;
  - and so on for the rest.
- **The grab-bag files are split by subject:** `fixes_test.go` (757 lines) and `toolloop_test.go`.
- **Fixtures:** the envelope fixtures become `testdata/` in the Signal adapter. Shared fakes go in `*test` helper packages: `signaltest`, a fake transport, a fake model.
- **No test is dropped or loosened to make a move pass.** Each step reports the count of test functions before and after. A test whose subject changes packages counts as moved, not removed.

## Line counts
- **Before:** 4,185 lines of non-test Go, at `14a2a21`:
  - `bot.go` 1,024, `chart.go` 782, `mcp.go` 689, `signal.go` 442, `openrouter.go` 409, `donut.go` 263
  - `names.go` 188, `main.go` 94, `log.go` 91, `config.go` 90, `mentions.go` 81, `journal_*` 32
- **After:** reported in the final PR with the same measure, by `go run ./internal/arch/sizes`. It leaves out tests, `internal/arch` and the `*test` helper packages, and shows core, adapters, kit and features apart, with the total and its change from 4,185.
- **Expected:** a total about the same or up to ~300 higher (the ports, `chat` types and router), with the core well under half of the total.

## Simplifications that change behaviour
Anything that would change what users see or how the bot is configured is listed with its line saving, and I ask before applying it. That covers wording, defaults, settings, limits, log lines and reply layout. The new `MODEL_API_URL` and `MODEL_API_KEY` settings are additions, and existing settings keep working. The "at least one network" check replaces "SIGNALCLI_SOCKET is required" with the same effect for current setups. Simplifications that keep behaviour the same go ahead, and the tests show it.

## Verification
- **Each step:** gofmt, vet, `go test -race ./...` (guardrails included), the offline Docker-style build, and the CI summary.
- **Feature isolation:** remove each feature's line from `features.go`. It still builds, the core tests pass, and only that feature's behaviour is gone.
- **Network isolation:** the core runs on a fake transport; the Signal adapter runs on envelope fixtures.
- **Privacy and logs:** the privacy and log tests run against the full bot.
- **End to end:** in Signal, a mention, a DM, a photo, a sticker, `/example`, a chat command, a line chart, a donut, a follow-up, group context and an MCP tool.
