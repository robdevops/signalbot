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

In a group, only those `GROUP_ADMINS` allows may change its alerts: by default the group's admins and mods (see [Who may use the bot](CONFIGURATION.md#who-may-use-the-bot)), or `all`, `none`, or listed IDs. Anyone there may see them with `/alerts` and open the menu; anything else, from someone it doesn't allow, is answered "Only <group>'s admins and mods can change its alerts." (with the group's name, and the roles `GROUP_ADMINS` names) (or "Changing this group's alerts isn't open to you." when no role may), and a refused button press shows the presser the same and changes nothing. In a DM, its owner changes their own alerts.

- `/alerts` shows which kinds are on and off in this chat, and which make a sound, and how to change them.
- `/alerts on` and `/alerts off` subscribe the chat to every kind, or stop it.
- `/alerts` and a kind (its `kind`, `label` or one of its `words`) turns that kind off, or on again; with alerts off, it turns them on for that kind alone.
- `/alerts sound on` and `/alerts sound off` give every kind a sound, or none; `/alerts sound` and a kind toggles that kind's (after `sound on`, a kind turned off is the exception: kinds the feed adds later have sound). Alerts without a sound arrive silently (no notification, where the network can do that).

On Telegram, `/alerts` answers with two menu buttons, **🔔 Alerts ▸** and, with some kind on, **🔊 Sound ▸**. Opening one (▾) hides the other and shows a toggle per kind under it: every kind under Alerts, the kinds that are on under Sound (before the feed has said its kinds, one toggle for all alerts, and one for their sound). A toggle that is on is highlighted (blue), one that is off struck through; a press redraws the menu in place, and pressing the open menu's button closes it. In a forum, each topic has its own alerts, changed by the forum's admins and mods as `GROUP_ADMINS` has it.

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
