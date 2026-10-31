# Contributing

The bot is a small core, adapters for chat networks and model providers, and features. These rules keep features out of the core; tests in `internal/arch` enforce the first, fourth and sixth.

1. **The core knows no feature, network or provider.** Only `cmd/signalbot` imports them. To add a feature, add a package under `internal/features/` and one line to `cmd/signalbot/features.go`; to remove one, delete both.
2. **Ports only.** A feature uses the `feature.Host` it is given: settings (`Env`), capabilities, media, commands, tool groups and hooks. It may import `internal/feature`, `internal/chat` and `internal/kit/*`, never core internals or another feature.
3. **A feature owns everything it needs:** its settings, commands, tools, prompt lines and tests, with a package comment describing it. Every setting it reads must be in the README's configuration table (a test checks).
4. **Shared code is a library.** Code that more than one package uses, with no knowledge of the bot, goes in `internal/kit/`, which imports nothing else internal. Code with one user stays with that user.
5. **Data-source adapters.** When a feature depends on the output shape of one particular MCP server, the parsing goes in `internal/sources/<server>`, with its own fixtures, so a change in that server's output is fixed there.
6. **Small files.** One role per file; a file over 500 lines, tests included, fails a test, and a package over 1,500 lines must split. `go run ./internal/arch/sizes` prints lines per package, and CI shows the change on each pull request.

A new chat network is an adapter in `internal/transports/` implementing `chat.Transport`; a provider with its own API is an adapter in `internal/llm/` implementing `llm.Model`. Test features with `internal/feature/featuretest`, and the core with a fake transport.
