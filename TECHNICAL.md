# Technical notes

## Design

- Go, standard library only; single static binary
- Talks JSON-RPC to `signal-cli daemon` over a unix socket or TCP (`listAccounts` once per connection to learn its own id)
- Reconnects by itself if signal-cli restarts or the connection drops, waiting 1s and then doubling up to 32s between attempts; the wait resets once a connection has stayed up for a minute, and requests in flight when it drops fail rather than hang
- x.ai Responses API (`/v1/responses`) over a shared keep-alive HTTP client that requires TLS 1.3, one retry on 429/5xx
- Bounded under load: a concurrency slot (`MAX_CONCURRENCY`) is taken before any goroutine starts, so messages it ignores cost nothing and a flood is dropped, not queued
- In-memory state only, all capped: history per chat (`HISTORY_TURNS`), number of chats (500), names, frame size (1 MiB) and reply length (4 messages)
- Message text is not logged by default: `LOG_MESSAGE_BODY=true` adds the text of each message that addresses the bot to the log (PRIVACY RISK). A message that is not a mention, a reply or a DM is never logged, whatever the setting. A DM from someone not in `SIGNAL_ALLOWED_SENDERS` is ignored too, but its sender's id is logged, never the name or text
- Treats sender-controlled text as untrusted: display names and quotes are sanitised before reaching the model or the log, and a name two people claim is never linked
- Configured from environment variables only; runs under systemd or Docker (hardened unit and image included; the image is pinned by digest and has no shell). Logs plain lines to stdout, with a timestamp only in a terminal (journald and Docker add their own)

## What the bot needs from signal-cli

Only the socket. The bot speaks JSON-RPC to it and never touches signal-cli's data directory, where the account keys live.

The socket's permissions decide who may connect, and the bot is let in through a group, not a user id, so a transient `DynamicUser` works:

- `signalbot.service` runs with `DynamicUser=yes` and `SupplementaryGroups=signal-cli`, so its throwaway user is in the group.
- Docker's `--group-add` puts the container's user in the same group. The container mounts only the socket's directory, so the data directory isn't there to read.
- signal-cli should create the socket group-writable (`0770`, from `UMask=0007`) in a directory the group can search (`0750`). A default umask gives `0755` and locks out even the group.

Everyone outside the group gets "Permission denied". The group is the only access control: anyone in it can drive the Signal account, so keep it to the bot.

The bot's keys stay out of reach in two ways. It runs as a different user from signal-cli, and the data directory is mode `0700`, so it can't be searched. That mode matters: `UMask=0007` makes files signal-cli creates group-accessible, and the bot is in that group, so with a `0755` directory it could read them (tested). Also, `ProtectHome=yes` hides `/home`, where signal-cli keeps its data by default (`~/.local/share/signal-cli`). Data kept elsewhere, such as `/var/lib/signal-cli`, relies on the directory mode alone; add it to `InaccessiblePaths=` in the unit to hide it outright.

A TCP socket (`--tcp 127.0.0.1:7583`) needs none of this, but then any local user can connect and control or spy on the Signal account (PRIVACY RISK).
