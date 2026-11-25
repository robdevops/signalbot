# Feeds

An MCP server can offer a **feed**: a resource listing items, such as weather warnings, price alerts, CI results or a sensor going out of range. The bot reads it every 30 seconds and posts each new item to the chats that asked for it, without the model. Add `feed` to the server's entry in the [MCP file](FILES_LINKS_TOOLS_MCP.md#mcp-connectors):

```json
"weather": {
  "url": "https://weather.example/mcp",
  "name": "Weather",
  "feed": {"resource": "weather://alerts", "command": "alerts"}
}
```

| Key | What it does |
|---|---|
| `resource` | the resource the bot reads (`resources/read`), required |
| `command` | the chat command for it; unset, `/alerts`. Two servers with feeds need different commands |

A server with a feed starts even with [no model](MODELS.md#no-model). A second feed is a second server entry, with its own URL or command and headers.

The bot reads the feed every 30 seconds, so an item reaches the chats at most about 30 seconds after the server lists it. It doesn't use MCP's push subscriptions: a small read every 30 seconds costs little, and works with every server.

## In chat

Alerts are on by default in a DM: every kind, silently, from the chat's first message to the bot (or its first `/alerts`), until someone there turns them off, which is remembered. A group starts with them off, so the bot never posts into a group unasked.

- `/alerts` shows whether alerts are on in this chat, which kinds are on and off, and how to change them.
- `/alerts on` and `/alerts off` subscribe the chat, or stop it.
- `/alerts sound on` and `/alerts sound off`: alerts are silent (no notification, where the network can do that) unless the sound is on.
- `/alerts` and a kind (its `kind`, `label` or one of its `words`) turns that kind off, or on again; with alerts off, it turns them on for that kind alone.

`/alerts` lists every kind, on or off. On Telegram, it answers with buttons that do the same: with alerts off, "Turn all on" and a button per kind (to have that one alone); with them on, "Turn off", sound on or silent, and each kind's ✅ or ▫️ to toggle it. A press redraws the menu in place; in a forum, each topic has its own alerts. Anyone who can address the bot in a chat can change that chat's alerts.

## The format

The resource's first content is text, a JSON object:

```json
{
  "identity": "station-42-2026",
  "kinds": [
    {"kind": "rain", "label": "Rain", "words": ["showers", "storm"]},
    {"kind": "wind", "label": "Wind"}
  ],
  "items": [
    {"id": "1043", "kind": "rain", "time": "2026-11-25T06:10:00Z", "expires": "2026-11-25T09:00:00Z",
     "text": "Heavy rain: 12 mm in the last hour", "link": "https://weather.example/1043",
     "chart": {"tool": "summary", "arguments": {"readings": ["rain"], "period": "1d", "as_of": "2026-11-25T06:10:00Z"}}}
  ]
}
```

| Field | What it is |
|---|---|
| `identity` | anything that changes when the server's IDs start over (a new log, a reset); the bot then treats every item as unseen |
| `kinds` | the kinds a chat may turn off, in the order the menu shows them (at most 20; a `kind` at most 32 bytes, without `/`, and a `label` cut to 32 characters). List only the kinds this server can send now |
| `items` | the items it still offers, oldest first. Keep the recent ones: the bot remembers the IDs of all those still listed, and of the last 1,000 others, and reads only the newest 1,000 listed, so an item listed again isn't sent again |
| `id` | unique within the `identity`, at most 200 bytes; required |
| `kind` | one of `kinds`; an item of a kind not listed always goes out |
| `time` | when it happened, RFC 3339 |
| `expires` | when it stops being news; unset, an hour after `time` |
| `text` | what the bot posts, in Markdown; an item without text is skipped |
| `link` | if set, posted under the text, cut to 500 characters |
| `chart` | a tool of the same server the bot calls, `{"tool", "arguments"}`, for the picture posted with it (drawn once per item). Put any time the picture must match, such as `as_of`, in the arguments. On Telegram, an alert with one picture and a caption that fits gets 🔄 under it, and with `"arg"` and `"values"` (such as `"arg": "period", "values": ["1d", "7d", "1m"]`) a button per value: a press draws the chart again and edits it in place, the alert's words kept |

An expired item is never sent, so the server decides what still matters: a passing warning can expire in minutes, a condition such as "battery low" can stay until it clears. On its first read the bot sends the items not yet expired (an item with neither `time` nor `expires` is only sent if it appears later). One read sends at most 10 new items; more are logged and the oldest skipped. A feed that can't be read is logged once, and again when it can be.

## State

The buttons under alerts' charts are kept with direct commands' (newest 300, in `mcp-posts.json`, with the alert's text and the chart's call). The bot keeps, in `mcp-feeds.json` in the state directory, each feed's `identity` and the IDs it has had, and the chats subscribed: their IDs, the kinds turned off and the sound setting, until `/alerts off`. See [PRIVACY.md](../PRIVACY.md). Without a usable state directory, feeds are off, with an error in the log.
