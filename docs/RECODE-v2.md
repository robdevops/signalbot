# Plan: modular recode, v2 (from `e40dc5f`)

## Goal
People can bolt features on cleanly, and the core never mirrors the shape of a feature, including features long removed. Nothing else is in scope: Signal and OpenRouter stay in the core, and there's no provider switching or multi-network support. Those can be planned separately later.

## The rule that decides every port
A port belongs in the core only if it describes a stage of handling a message: a message arrives, the model is asked, a reply goes out, a chat is forgotten. A hook may return generic actions. No field, type or string may exist for one feature. Test: delete any feature, and the core is unchanged.

## Ports (`internal/feature`)
```go
type Feature interface{ Register(h Host) error }

type Host interface {
    Env() Env                                       // settings the feature reads; names recorded for the docs test
    Info() Info                                     // bot name, model, the offered tools' names
    Media(ctx, m Message, i int) ([]byte, error)    // fetch a message's image or sticker
    Tools(g ToolGroup)                              // tools, the prompt fragment naming them, and Offer(Turn) bool
    OnMessage(func(ctx, Message) Action)            // every message, addressed or not
    OnTurn(func(ctx, Turn) Additions)               // before the model is asked
    OnReply(func(Turn, Reply) Reply)                // before the reply is sent
    OnForget(func(chat string))                     // when a chat's history is erased
}
type Action    struct{ Answer string; Require string; Remember string } // zero value: carry on as usual
type Additions struct{ Prompt, ModelNote, HistoryNote string; Images []string }
type Reply     struct{ Text, HistoryNote string; Pictures [][]byte; Used []string }
```
- **Commands** are a helper in `internal/feature` (`feature.Command(h, name, fn)`) built on `OnMessage`, not a core concept.
- **A tool group's prompt fragment is its own whole text** (for example "MCP tools by server, …, and name them by server"). The core joins the fragments and adds a neutral sentence.
- **The core's history note for a reply** is only the reply text plus whatever `OnReply` set in `HistoryNote`. It has no "chart" or caption wording.
- **Unaddressed messages** reach `OnMessage` without being logged. A hook may return `Remember`, which the core keeps under its own expiry rules.

## Core (`internal/bot`, one package)
- The Signal client, names and mentions, message decisions and the reply split.
- The OpenRouter client and the tool loop.
- History, including the disappearing-message expiry.
- Config, logging, the base prompt and `INTRO`.
- The registry calling the hooks in feature order.

It uses as few files as reads well, with no line limit.

## Features (`internal/features/<name>`, one line each in `cmd/signalbot/features.go`)
| Feature | Uses |
|---|---|
| `example` | the command helper |
| `websearch` | `Tools` |
| `charts` | `Tools` with `Offer`; `OnReply` to send the caption in place of the text and set its history note; its own follow-up state, cleared in `OnForget` |
| `mcp` | `Tools`, and the command helper for `chat_commands`, whose action is `Require` |
| `groupctx` | `OnMessage` returning `Remember` |
| `vision` | `OnTurn` with `Media` |

Code with two or more users goes in `internal/kit`; code with one user stays with it.

## Fixes from the first recode, carried over
- A golden test of the full system prompt, written before any move, which must stay byte-identical.
- Message handling stays two-phase: a cheap decision on the reader goroutine, and anything that may call the network (waiting for the account ID, reloading contacts, sticker labels) only after a concurrency slot is taken.
- The names used to turn @Name into mentions are forgotten with the chat's history (`OnForget`).
- A command name another feature has is refused at registration, not by a hard-coded name.
- A file-splitting tool must keep multi-line constants. Check that no line is lost after every move (the first recode silently dropped two tool descriptions).
- Shared helpers must not import the transport, which caused an import cycle in the first recode.
- A test that starts slow replies drains them before it ends, or their log lines land in the next test.
- Tests that change the DM allowlist do it in place, because the transport holds a reference to the map.
- Docker: the `.dockerignore` and `COPY` include the packages, the build targets `./cmd/signalbot`, and the README's build step matches.

## Guardrails (Go tests)
- **Imports:** only `cmd/signalbot` imports features; a feature imports only `internal/feature`, `internal/kit` and its own packages; no feature imports another.
- **No feature names in the core:** the core's source mentions no feature name or feature-only word (chart, donut, MCP, group context, `/example`).
- **Each feature alone:** each feature's tests run on a fake host.
- **Docs:** every setting read is in the README's configuration table.
- **Sizes:** reported on each PR, not enforced.

## Steps (behaviour unchanged at every step; tests move with their code; the test count is reported)
1. The golden prompt test and the guardrails.
2. `internal/feature` with the ports above, and the core calling them.
3. Move the features out one at a time, removing the core's copy each time: `example`, `websearch`, `groupctx`, `vision`, `mcp`, `charts`.
4. Docs: TECHNICAL.md (core, then one paragraph per feature) and CONTRIBUTING.md (the rule above).

## Expected size
About +300 to +500 lines over 4,185: the ports, the registry, and a package per feature. The core keeps today's structure minus the feature code.
