# Plan: modular recode — a minimal core, features behind ports

## Context
signalbot is one flat `package main` of about 4,000 lines of code and 3,700 of tests. Feature logic has leaked into the core:
- **`bot.go` (980 lines)** holds the chart follow-up counter, chart captions, image and sticker fetching, group chatter, `/example` and the system-prompt tool line.
- **`openrouter.go`** dispatches chart tools by name, and has a chart context key.
- **`main.go`** wires MCP straight into the LLM client.

The goal is a clean-slate split. The core is only what every deployment needs to answer a message, and everything optional is a feature in its own folder behind small ports. **Behaviour doesn't change:** tests move with their code and pass at every step.

## What is core, and why
The test for core: **with no features registered, the bot still answers a mention or an allowed DM with a model reply, remembers the conversation, and forgets it on time.**

| Core package | Why it is core |
|---|---|
| `internal/signal` | The transport: JSON-RPC, envelopes, send, typing, attachments and stickers as raw media calls, contacts |
| `internal/bot` | Deciding whether a message is for the bot (mention, reply, allowed DM), the allowlist, dedupe, concurrency, typing, the respond loop, shutdown, and command dispatch as a mechanism with no commands of its own |
| `internal/llm` | The OpenRouter client, capability check, and a tool loop that runs whatever tools are registered |
| `internal/history` | Per-chat memory with expiry (24h, `HISTORY_TURNS`, disappearing-message timers): a privacy promise, so it's core and tested centrally |
| `internal/names` | Contact names and mention resolution, needed to show the model who's talking |
| `internal/reply` | Splitting, truncation, self-tag stripping, outgoing mentions |
| `internal/config`, `internal/logx` | Core settings, log levels, journald, and the rule that message text is never logged |
| `internal/feature` | The ports: the only thing features import from the core |

**Moved out of the core, unlike your example:**
- **MCP becomes a feature.** It's an optional tool provider configured by `MCP_CONFIG`. To the core it is just "registers tools", the same as charts. Its protocol client (stdio, HTTP, SSE) is reusable code with no bot knowledge, so it lives in `internal/kit/mcp`, and `features/mcp` wires it in.
- **Web search becomes a feature.** It's one tool entry plus `MODEL_SEARCH`.
- **The intro becomes a feature.** "If asked to introduce yourself…" and `INTRO` are a prompt line. It gets tool names through the port, so `{tools}` keeps working.
- **No `sources/` package yet.** Nothing parses a specific MCP server's output today. The rule stays in CONTRIBUTING for when something does.

## Features (`internal/features/<name>`, one line each in `cmd/signalbot/features.go`)
- **`mcp`**: loads `MCP_CONFIG`, starts servers, registers their tools, and supplies the "MCP tools by server" prompt text. It also owns `chat_commands`, built on the current code first (`14a2a21`): it registers `/name` commands that run a forced-tool turn, the bare-command reply for `require_arg`, and their startup checks. This needs one extra port, `Host.ForceTool(turn, tool)`, or a `Command` variant that returns "run the model with only this tool, required".
- **`websearch`**: the `openrouter:web_search` tool, toggled by `MODEL_SEARCH`.
- **`vision`**: fetches photos, quoted photos and stickers into the turn when the model can see images. It also writes the history marker that says images aren't kept.
- **`groupctx`**: `GROUP_CONTEXT`, keeping unaddressed group messages as context.
- **`charts`**, as sub-packages:
  - `line` (`make_chart`) and `donut` (`make_donut_chart`): tool definitions and argument parsing;
  - `offer`: the trigger words, plus 2 follow-ups, cleared when history is forgotten;
  - `caption`: the one-message captioned reply and the "[sent the chart …]" note.
- **`example`**: `/example`, the template for your own commands.
- **`intro`**: the introduction prompt line and `INTRO`.

Shared pure code goes in `internal/kit/`: `draw` (canvas and fonts), `linechart`, `donut` and `mcp`. Kit code imports nothing internal.

## Ports (`internal/feature`): the smallest set today's features need
```go
type Feature interface{ Name() string; Register(h Host) error }

type Host interface {
    Env() Env                          // Bool/Int/String with defaults; names recorded for the docs check
    Caps() Caps                        // model sees images / calls tools
    Log() Logger                       // levels only; message text can't be passed
    Media() Media                      // GetAttachment, GetSticker, StickerLabel
    Command(name string, run CommandFunc)       // example
    Tool(t Tool)                       // def, Offer(Turn) bool, Run(ctx, args) Result{Text, Images+Captions}
    Prompt(func(Turn) string)          // intro, mcp
    Incoming(func(ctx, *Turn))         // vision: images and notes for the model and for history
    Unaddressed(func(Message))         // groupctx
    Replied(func(Turn, Completion, *Outgoing))  // charts: captions, follow-up count
    Forgotten(func(chat string))       // charts: drop per-chat state with the history
    Remember(chat string, m Msg)       // groupctx: add to history through the core's expiry
}
```
- The core builds "Your tools: …" from the names of the tools offered that turn, so it names no tool itself.
- Each feature's state stays inside the feature. The core only promises `Forgotten`, so features can't outlive the privacy expiry.

## Guardrails: Go tests, so `go test ./...` enforces them locally and in CI
- **`internal/arch` imports test** (go/parser, no new dependencies):
  - no core package imports `internal/features`;
  - `internal/kit` imports nothing internal;
  - no feature imports another;
  - a feature imports only `internal/feature`, kit and the stdlib.
- **File-size test:** every `.go` file, tests included, fails at more than 300 lines. Every package must stay under 1,500 lines.
- **Isolation test:** the core tests also run with an empty feature list. Each feature's tests run with only that feature registered.
- **Docs test:** every env var a feature reads through `Env()` must appear in the README config table.
- **Sizes:** `go run ./internal/arch/sizes` prints lines per package. CI writes the table and the change from the base branch to the job summary. A summary needs no extra token permissions, unlike a PR comment.
- **CI (new):** `.github/workflows/ci.yml` runs gofmt, vet, `test -race` and sizes.

## Steps (one PR each; behaviour unchanged at every step)
1. **Guardrails and CI**, with the arch rules skipped until their packages exist.
2. **Split into core packages**, and move main to `cmd/signalbot`. Test helpers go to `internal/signal/signaltest`. Update the Dockerfile (`COPY cmd internal`, build `./cmd/signalbot`).
3. **kit:** `draw`, `linechart`, `donut` and `mcp`, as pure code with their tests.
4. **Ports and registry**, with the core calling the hooks.
5. **Move the features out**, deleting the core's copy each time, in this order:
   1. `example`
   2. `intro`
   3. `websearch`
   4. `mcp`
   5. `groupctx`
   6. `vision`
   7. `charts`

   Turn on each arch rule as soon as it can pass.
6. **Docs:**
   - TECHNICAL.md covers the core, with one short paragraph per feature linking to that feature's `README.md` in its folder.
   - CONTRIBUTING.md states the rules.
   - The README stays as it is for users.

## Line counts
- **Before:** 4,185 lines of non-test Go, at `14a2a21`:
  - `bot.go` 1,024, `chart.go` 782, `mcp.go` 689, `signal.go` 442, `openrouter.go` 409, `donut.go` 263
  - `names.go` 188, `main.go` 94, `log.go` 91, `config.go` 90, `mentions.go` 81, `journal_*` 32
- **After:** reported in the final PR with the same measure, by `go run ./internal/arch/sizes`. It counts non-test `.go` files and leaves out the utilities `internal/arch` (the guardrail tests and the sizes tool) and `internal/signal/signaltest`. The table shows core, kit and features separately, plus the total and the change from 4,185.
- **Expected:** a total about the same or a little higher, since the ports add roughly 150–250 lines and removing duplication takes some back. The core alone should come in well under half of today's total.

## Simplifications that change behaviour
Any simplification found during the recode that would change what users see or how the bot is configured is not made silently. That covers wording, defaults, settings, limits, log lines and reply layout. The PR that finds one lists it with its line saving, and I ask before applying it. Simplifications that keep behaviour the same (dead code, duplication, clearer structure) go ahead, and the tests show nothing changed.

## Verification
- Each step: gofmt, vet, `go test -race ./...` (arch, size, isolation and docs tests included), and the CI summary.
- Remove each feature's line from `features.go` in turn: it still builds, the core tests pass, and only that feature's behaviour is gone.
- The privacy and log tests run against the full bot.
- **End to end:** an offline Docker build, then in Signal a mention, a DM, a photo, a sticker, `/example`, a line chart, a donut, a follow-up, group context and an MCP tool.
