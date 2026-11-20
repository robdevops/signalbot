# GeoGuess

A GeoGuessr-style game for signalbot. `/geo` posts a street-level photo;
players answer by sending a location (Signal's 📎 → Location with the pin
moved, or typed coordinates such as `-37.81, 144.96`); when time is up the
bot names the place and ranks players by how far off they were.

```
/geo                 start a round (GEO_ROUND, default 3 minutes)
/geo 90s | /geo 5m   start a round of that length (30s to 30m)
/geo europe          a round in a continent or countries (/geo AU,NZ); combines with a length
/geo end             finish now and reveal (only the player who started it)
/geo skip            cancel and reveal without scoring (only the player who started it)
/geo scores          this chat's totals over the last 7 days
```

Nobody needs to mention the bot: a message that starts with `/geo`, and any
guess during a round, reaches it as it is.

It is a separate program that signalbot starts as an
[MCP addon](../../docs/FILES_LINKS_TOOLS_MCP.md#addons) server. It speaks MCP
2025-06-18 over stdio (newline-delimited JSON-RPC) by hand, with signalbot's
addon extension, and uses no MCP library. It needs no model: its commands and
guesses never reach one.

## Install

Build it as a static binary, which the Docker image needs (it has no shell
or runtimes) and systemd is happy with:

```
CGO_ENABLED=0 go build -o /opt/signalbot-addons/geoguess ./cmd/geoguess
```

Mount it read-only: under Docker, add `- /opt/signalbot-addons:/addons:ro` to
the volumes in `compose.yaml`; under systemd, use the host path, or bind it
with `BindReadOnlyPaths=/opt/signalbot-addons:/addons` in a drop-in. Then add
it to the MCP servers file (`MCP_CONFIG`). The server must be called `geo`,
which is what gives it the `GEO_` settings below:

```jsonc
"geo": {
  "command": "/addons/geoguess",
  "chat_commands": {"geo": {"tool": "geo", "direct": true, "example": "/geo 3m"}},
  "addon": {"chats": ["<group id>"], "post": true, "react": true, "watch": true, "watch_max": "1h"}
}
```

`chats` takes the groups' IDs as Signal or Telegram shows them, or `["all"]`;
add `"dms": true` to play in DMs too. GeoGuess needs `post` (for the results),
`react` (the 📍 on a counted guess) and `watch` (to read guesses). A round
lasts at most 30 minutes, so the default `watch_max` of 1h is enough.

GeoGuess keeps its own files in `GEO_STATE_DIR`, which you create, writable by
the bot's user, and set in `/etc/signalbot.env`:

- **systemd:** in a drop-in (`sudo systemctl edit signalbot`), add
  `StateDirectory=geoguess` (the unit's `DynamicUser=` makes it and owns it),
  and set `GEO_STATE_DIR=/var/lib/geoguess`.
- **Docker:** `sudo install -d -o 65532 -g 65532 -m 700 /var/lib/private/geoguess`,
  add `- /var/lib/private/geoguess:/geo` to the volumes, and set
  `GEO_STATE_DIR=/geo`.

It shares the bot's memory limit, so allow for `GEO_MEMLIMIT` in `mem_limit`
or `MemoryMax`.

## Settings

signalbot passes an addon server called `geo` every `GEO_` variable from its
environment file, plus `PATH`, `HOME` and, if set, `TZ` (the round's deadline
is written in it), `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`, `SSL_CERT_FILE` and
`SSL_CERT_DIR` (so it reaches the photo sources through the bot's proxy, and
trusts its CA), and nothing else.

| Variable | Default | Description |
|---|---|---|
| `GEO_STATE_DIR` | — (required) | Where GeoGuess keeps `geo.json` and its log. Create it, writable by the bot's user; GeoGuess exits at start if it can't list and write it |
| `GEO_MAPILLARY_TOKEN` | empty | Mapillary client token (`MLY\|…`), sent as `Authorization: OAuth <token>`. Empty means Panoramax only, and then rounds are in France unless `GEO_REGIONS` says otherwise |
| `GEO_MEMLIMIT` | `200MiB` | Memory limit, applied with `debug.SetMemoryLimit`, written as `GOMEMLIMIT` is (`B`, `KiB`, `MiB`, `GiB`, `TiB`) |
| `GEO_ROUND` | `3m` | Default round length, 30s to 30m |
| `GEO_REGIONS` | empty (world) | Continents (`europe`, `asia`, `africa`, `north-america`, `south-america`, `oceania`) or ISO country codes, comma-separated |
| `GEO_MIN_QUALITY` | `0.5` | Minimum Mapillary `quality_score` (0 to 1), when a photo has one |
| `GEO_MAX_AGE_YEARS` | `10` | Ignore older photos |
| `GEO_BUFFER` | `3` | Checked photos kept ready (1 to 20) |
| `GEO_NOMINATIM_URL` | `https://nominatim.openstreetmap.org` | Reverse geocoder for the place names; `https://` only, as it is sent each round's answer |
| `GEO_PANORAMAX_URL` | `https://api.panoramax.xyz` | Panoramax API (a STAC catalogue), such as one instance's; `https://` only |
| `GEO_MAPILLARY_URL` | `https://graph.mapillary.com` | Mapillary Graph API; `https://` only |

It reaches `graph.mapillary.com` and Mapillary's image CDN, `api.panoramax.xyz`
and the Panoramax instances' image hosts, and the Nominatim server, over
TLS 1.3.

## Privacy

GeoGuess gets only what signalbot's addon block allows: the text after
`/geo`, and during a round the messages that look like a location (a map link
or coordinates), each with the chat and sender as pseudonyms and the time the
bot got it. It never sees a phone number, account ID, group ID or, unless
`names` is set, a display name; it writes players as `{u_…}` and the bot puts
their names in. Guesses are claimed by the bot, so they are never answered,
sent to the model or kept in the chat's history.

What it sends out is about photos, never about players: map areas to search
and photo IDs to Mapillary and Panoramax, and the answer's coordinates to
Nominatim for the place's name, each with the user agent
`geoguess (+https://github.com/robdevops/signalbot)`.

A guess deleted for everyone afterwards is not passed on to GeoGuess, which
keeps only its distance anyway.

### What it keeps

`$GEO_STATE_DIR/geo.json`, rewritten whole on each change:

- open rounds: the chat's pseudonym, who started it, its deadline, the answer
  and its credit, and for each player (a per-chat pseudonym) only how far
  their last guess was and when. Where anyone guessed is never kept, in
  memory or on disk;
- the keys of the last 1,000 photos used, so none comes round again soon;
- 7 days of points per chat, by player pseudonym;
- outbox ops the bot hasn't read yet, for at most an hour.

`$GEO_STATE_DIR/geoguess.log` (moved to `geoguess.log.1` past 4 MiB at start)
records rounds started and revealed with their guess counts, photos
rejected by reason, and API errors by status. It never holds a guess, a
coordinate, a place or person's name, a pseudonym or message text.

## The protocol

- `initialize` advertises `tools`, `resources.subscribe` and
  `experimental["io.github.robdevops.signalbot/addon"] = {"version": 1}`.
- `tools/list` has `geo` (`{args}`) and `geo_guess` (`{text}`), both with
  `_meta["io.github.robdevops.signalbot/visibility"] = ["bot"]`, so neither is
  offered to the model.
- `tools/call` reads the chat, the sender and the receive times from
  `_meta["io.github.robdevops.signalbot/context"]`. Starting a round answers
  with the photo, its caption and a `…/watch` for `geo_guess`, until 5s past
  the deadline; a recorded guess answers with `…/react: "📍"`.
- When a round's time is up GeoGuess queues a `post` with the results, its
  players as `{u_…}` placeholders, and an `unwatch`, and sends
  `notifications/resources/updated` for `signalbot://outbox` if subscribed.
  `/geo end` and `/geo skip` answer with the results themselves and queue
  only the `unwatch`.
- `resources/read` of `signalbot://outbox/<cursor>` returns
  `{"cursor", "more", "ops"}`. A cursor is `<boot-nonce>.<seq>`; reading one of
  this run lets go of the ops up to it, and any other cursor, such as `0` or
  one from before a restart, gets everything still held. Op IDs survive a
  restart, so the bot can skip those it already applied.
- On start, rounds still open get their timers back, and those whose time
  passed while GeoGuess was down are revealed at once.
