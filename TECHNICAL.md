# Technical notes

## Design

- Go, standard library only; single static binary
- Talks JSON-RPC to `signal-cli daemon` over a unix socket or TCP (`listAccounts` once per connection to learn its own id)
- x.ai Responses API (`/v1/responses`) over a shared keep-alive HTTP client that requires TLS 1.3, one retry on 429/5xx
- Bounded under load: a concurrency slot (`MAX_CONCURRENCY`) is taken before any goroutine starts, so messages it ignores cost nothing and a flood is dropped, not queued
- In-memory state only, all capped: history per chat (`HISTORY_TURNS`), number of chats (500), names, frame size (1 MiB) and reply length (4 messages)
- Treats sender-controlled text as untrusted: display names and quotes are sanitised before reaching the model or the log, and a name two people claim is never linked
- Configured from environment variables only; runs under systemd or Docker (hardened unit and image included; the image is pinned by digest and has no shell). Logs plain lines to stdout, with a timestamp only in a terminal (journald and Docker add their own)

## How the socket is shared

signal-cli and the bot talk over a unix socket, and the socket's permissions decide who may connect. Neither side can use `DynamicUser`, because a throwaway user id has nothing stable to be granted access:

- `signal-cli.service` runs as the fixed user `signal-cli`, with a `RuntimeDirectory` (`/run/signal-cli`, mode `0750`, so the group can search it) and `UMask=0007`, so the socket is created `0770`. A default umask would make it `0755`, which locks out even the group.
- `signalbot.service` runs as user `signalbot` with `SupplementaryGroups=signal-cli`.
- Docker's `--group-add` puts the container's user in the same group.

Everyone else gets "Permission denied". The `signal-cli` group is the only access control: anyone in it can drive the Signal account, so keep it to the bot.

A TCP socket (`--tcp 127.0.0.1:7583`) needs none of this, but then any local user can connect.
