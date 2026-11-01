# Contributing

The bot is a small core and features on top. [TECHNICAL.md](TECHNICAL.md) describes the design and what each does.

```
main.go, features.go    the main package at the module root: wiring, and one line per feature
internal/signal/        the chat network: the signal-cli client, decoding, sending, names; only main imports it
internal/model/         the model client (OpenRouter or another chat completions API) and the tool loop
internal/chats/         per-chat state: history, dedupe, mention names, the mailbox
internal/timers/        one-shot timers
internal/config/        settings from the environment; config.Test, the test hooks
internal/bot/           the decision, the prompt, the hooks; wires the core, and reaches Signal through Network
internal/feature/       the ports and shared types, the command helper, featuretest
internal/features/*     one package per feature
internal/kit/*          code with two or more users and no bot knowledge
```

## The port rule

Features reach the core only through the ports in `internal/feature`, one per stage of the core's lifecycle:

| Stage | Port |
|---|---|
| A message arrives | `OnMessage`: carry on, ignore it, answer without the model, choose a tool, or keep text as context |
| The model is asked | `OnTurn` (a system-prompt line, a note on the message, images) and `Tools` (a `ToolGroup`) |
| The reply goes out | `OnReply`: change the text or pictures |
| Time arrives | `At`: a one-shot timer |

and the host's services: `Env`, `Info`, `Media`, `Deliver`, `Groups`, `Redact` and `Go`. Hooks run in `features.go` order. An `OnMessage` hook for an unaddressed message runs on the reader goroutine, so it must not block or call the network.

If a feature can't be built on these, that's a design finding. Either a port becomes more generic, and stays tied to its stage, or the feature takes on more of the work. A feature-shaped field in the core is never the answer: delete any feature and the core doesn't change.

## Plain Go, no ceremony

- Use concrete types and structs freely. Add an interface only where there are two real implementations, or a test needs a fake. The one there is, `bot.Network`, is there for the bot's in-memory fake network.
- Define each type once and use it everywhere, rather than copying it per package. `Message`, `Media`, `Picture`, `Turn` and the port types live in `internal/feature`, and the core uses them directly.
- When a function needs many inputs, pass a typed struct, never a map or a long list of positional arguments.
- No wrappers or adapters that only pass calls through.
- Files by role, no line limit. Kit gains a package only when a second user appears.

## Guardrails

Tests in `internal/arch` enforce these:

- **Imports:** only the main package, at the module root, imports features and `internal/signal`; a feature imports only `internal/feature`, `internal/kit` and its own packages; `internal/signal` imports only `internal/feature`, kit and `internal/config`; `internal/config` and kit import no other internal package.
- **No feature names in the core:** the core's source mentions no feature name or feature-only word (chart, MCP, group context, `/example`, `/schedule`, …).
- **Docs:** every setting read, the core's or a feature's (`h.Env("NAME", …)`), is in the README's configuration table.

And by convention:

- **Zero features:** the core's own tests run with no features registered, on an in-memory fake network.
- **Each feature alone:** each feature's tests run on `featuretest`, a fake host.
- **Scenarios:** `scenario_test.go` drives the whole bot through a fake signal-cli socket and a fake model API (OpenRouter's, or another provider's), and checks no message text reaches the log. The golden prompts in `testdata` pin the system prompt byte for byte (`go test . -run Golden -update` rewrites them).
- **One test hook:** what a test needs to change inside the bot goes in `config.Test` (nil in production), never in an exported variable or method. The only other exports for tests only read: `logx.SetOutput` and `logx.Buffer` capture the log (kit can't import config), and `config.Read` serves the docs guardrail.
- **Sizes:** `go run ./internal/arch/sizes` reports lines per package; CI reports them on each PR, not enforced.
- **Never log message text,** including in errors and panics. `logx` escapes line breaks; don't log what a user wrote.
- Tests use only the names Alice and Bob.

Before sending a change: `gofmt -l .`, `go vet ./...` and `go test -race ./...`, with `GOTOOLCHAIN=local`. No new dependencies.

## Adding a feature

1. Make `internal/features/<name>` with a `Register(h feature.Host) error`. Read settings with `h.Env`, register hooks and tool groups, and return an error only for a config error, which stops the bot.
2. For a command, use `feature.Command(h, "name", fn)`. A name another feature has taken is refused. If the model should know the command, name it in your tool group's `Names` and `Listing`.
3. For tools, call `h.Tools` with a `ToolGroup`: `Names` for the intro's `{tools}`, `Listing` for the tools line, `System` for a prompt line while offered, and `Keywords` to offer it only when a message matches.
4. Test it on `featuretest`, without the core.
5. Add one line to `features.go`. Order matters: it is the order hooks run in and of the tools line.
6. Add its settings to the README's configuration table, and a row in TECHNICAL.md's features table and a paragraph under it.

`internal/features/example` is the smallest feature to copy.
