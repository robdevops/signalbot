# Technical notes

## Design

- Go, standard library only; single static binary
- Talks JSON-RPC to `signal-cli daemon` over a unix socket or TCP (`listAccounts` once per connection to learn its own id)
- Reconnects by itself if signal-cli restarts or the connection drops, waiting 1s and then doubling up to 32s between attempts; the wait resets once a connection has stayed up for a minute, and requests in flight when it drops fail rather than hang
- Exactly one provider, chosen by which key is set (`OPENROUTER_API_KEY` or `XAI_API_KEY`; both or neither is a startup error), over a shared keep-alive HTTP client that requires TLS 1.3, with one retry on a network error, 429 or 5xx. OpenRouter uses chat completions with `provider.data_collection: deny` (only providers that do not collect the data) and asks for the real cost to be reported. x.ai uses the Responses API (`/v1/responses`), and every request sets `store: false`, so x.ai is asked not to keep the conversation
- Bounded under load: a concurrency slot (`MAX_CONCURRENCY`) is taken before any goroutine starts, so messages it ignores cost nothing and a flood is dropped, not queued
- Never swapped: `MemorySwapMax=0` in the unit and `--memory-swap` equal to `--memory` in Docker keep conversation history out of swap (cgroup v2 with swap accounting; no capability needed)
- In-memory state only, all capped: history per chat (`HISTORY_TURNS`), number of chats (500), names, frame size (1 MiB) and reply length (4 messages)
- Message text is never logged. A message that is not a mention, a reply or a DM is not logged at all. A DM from someone not in `SIGNAL_DM_USER_IDS` is ignored too, but its sender's id is logged, never the name or text
- The same message repeated by the same sender in the same chat within 10 seconds is ignored
- Honours disappearing messages: a chat's timer (`expiresInSeconds`) is stored with its history. No message is kept longer than 24 hours, or than that timer if shorter; expired messages are dropped from memory when their time is up (and ignored by any read before then), and the garbage collector reclaims them
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
