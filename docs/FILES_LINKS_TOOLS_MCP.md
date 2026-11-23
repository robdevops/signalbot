# Files, Links, Images, Tools, and MCP connectors

- [Files](#files)
- [Links](#links)
- [Images](#images)
- [How tools are offered](#how-tools-are-offered)
- [MCP connectors](#mcp-connectors)
- [Addons](#addons)

## Files

Send the bot a file in a DM, attach one when you @mention it or reply to it, or @mention it in reply to someone else's file. Each reaches the model in the form it can use:

| File | What the model gets |
|---|---|
| PDF | The PDF itself. A model that reads files natively gets it as it is; for any other, OpenRouter reads it first with its `mistral-ocr` engine, which handles tables, columns and scanned pages |
| Audio, including voice notes | The audio, if the model takes audio (OpenRouter's model list says); otherwise a note that it can't hear it |
| Text and code (`.txt`, `.md`, `.csv`, `.json`, source files, …) | Its text, up to 50,000 characters |
| Word, Excel, PowerPoint (`.docx`, `.xlsx`, `.pptx`) | Its text: paragraphs, cells by sheet (each headed with its name), slides in order |
| Anything else | A note that it was attached and can't be read |

Set `FILES=off` to stop files reaching the model, for cost or because documents shouldn't go to the model's provider: they are never fetched, and the model is told only that a file was attached and that reading files is turned off. Images and stickers aren't files here: they still go to a model that takes images.

Up to 4 files a message, 12 MB each and 16 MB together (the model is told of any left out). A Word, Excel or PowerPoint file is read within fixed limits: one with over 20,000 parts or elements nested 256 deep is reported as unreadable, and past 32 MB of XML (a deck heavy with shapes can reach it) the text read so far is shown, and the model is told only the first part is there. The model is told that what a file says is content to read, not instructions, and tools without keywords aren't offered on a message whose file the model is shown (its text, a PDF or audio; see [How tools are offered](#how-tools-are-offered)). **OCR costs money:** OpenRouter charges about $2 per 1,000 pages, so a 150-page filing is about $0.30 each time it's sent, on top of the reply, and shows in the reply's logged cost. The bot never keeps a file, so asking about it again means replying to it again, and it's read again. To fetch the original of a file someone else posted, the bot remembers recent attachments' IDs and names in memory for a day; see [PRIVACY.md](../PRIVACY.md). The log records each file's kind and size and what the model got, or that it was no longer available, never its name.

## Links

A message that addresses the bot and has a URL, or replies to a message with one, gets the page read for the model: up to 5 links (any more are named to the model as not opened, so it can say to send them on their own), the page's main text up to 15,000 characters, a PDF link as a PDF (see [Files](#files)). Only links a person sent are fetched, never ones the model or a page suggests, so a link in a reply to one of the bot's own messages isn't fetched. Only links on the standard web ports (80 and 443) are fetched. A link to a private or local address (the bot's own machine, your network, a cloud metadata service) is refused, however its name resolves, and the model is told it couldn't load it. So is a link that looks private or one-time, so asking "is this legit?" can't spend it: a reset, confirmation, unsubscribe or webhook link, or one carrying a token, key or signature, including where a short link redirects. Each chat can have 20 links fetched an hour; past that the model is told when links can be read again. A link that fails says why in plain words, such as "the page wasn't found (404)" or "the site refused the request (403); it may need signing in or block bots". The log records what came of each link (its time, size and kind, how much text was read, a PDF attached, or why it failed or wasn't fetched), never the link itself, not even its host. The model is told that what a page says is content to read, not instructions, and tools without keywords aren't offered on a message whose page the model is shown (see [How tools are offered](#how-tools-are-offered)).

## Images

Asked for a picture (a drawing, painting, sketch, illustration, art in any style, a diagram, a logo, an icon, a poster, a cartoon), the model can make one with an image model on OpenRouter: [GPT Image 2](https://openrouter.ai/openai/gpt-image-2) by default (`IMAGE_MODEL`), at `IMAGE_QUALITY` (default `medium`). The model writes the whole prompt itself, as the image model sees nothing else, and the picture is sent with its reply. It says whether the picture is technical (a diagram, cutaway, cross-section, schematic, infographic, or a labelled, how-it-works or educational picture) or artistic. For a technical one it first works out the facts the picture must get right (what each label says and points to, which way arrows point, angles, counts, order) and lists them; the bot then asks the image model for an accurate picture in whatever style was asked for, or if none a rich infographic (a bold title, a detailed main illustration, colour-coded sections, icons, callouts with the facts and figures, a legend), with those facts as must-haves and rules for short, exact text and nothing invented. An artistic prompt goes as the model wrote it. History keeps a note of the prompt, not the picture, and for a technical picture its requirements too, so "make it bluer" or "add the other planets" works by the model writing a new prompt that keeps them (still to scale, say). A chart of data is still the chart tools' job: the image tool's description says so. The tool is offered only on messages with words such as draw, sketch, paint, illustrate, art, diagram, cutaway, cross-section, schematic, exploded view, blueprint, flowchart, org chart, mind map, visualise, image, picture, photo, logo, icon, poster, banner, meme, avatar, cartoon or render, then for `TOOL_LINGER_ROUNDS` more replies; with words both share, such as visualise or org chart, both it and the charts are offered and the model picks. It makes new pictures only: it can't edit one you send. If the model's reply fails after a picture is made, the picture is sent alone. Each picture's cost (logged with the reply) counts toward `DAILY_BUDGET`. Only OpenRouter's API makes pictures; with another, or `IMAGE_MODEL=off`, the tool isn't offered. A prompt the image model's provider refuses under its content policy is reported to the user as such; other failures as the service failing.

## How tools are offered

Web search and tools without `keywords` are offered with every message, except that on a message whose page or file the model is shown (its text, a PDF or audio), tools without `keywords` are left out (web search and a chat command's own tool stay), so text inside a document can't call a tool: only what someone typed can. Charts, images, and MCP servers with `keywords`, are offered only when a message matches their words, then for `TOOL_LINGER_ROUNDS` more replies, and each reply that matches or uses one of their tools starts that count again. So "chart NVDA", "add AMD", "make it a year" keeps the charts on however long it runs; only replies that leave them unused count down.

The default of 2 is meant to cover a short aside ("thanks!", "what's AMD's ticker again?") before you come back to the chart, without the tools still being on when the conversation has moved on. Lingering longer has two costs: a tool on for unrelated messages is one the model may use when it shouldn't (a stock tool picked for a population question), and its descriptions add to every request. The prompt cache gains little, because the offered tools change once either way when a group drops off. Raise it if people often come back to a chart after several other questions; set `0` to offer a group only on messages that name it.

## MCP connectors

The model can call tools from [MCP](https://modelcontextprotocol.io) servers, remote (`url`) or local (`command`). Anyone who can address the bot can make it call them (for [addons](#addons), their `chats` and permissions are the gate), so choose what each server offers: `allowed_tools` offers only the tools listed, `blocked_tools` offers all but those, and without either every tool the server has is offered. Put the servers in a file, for example `/etc/signalbot-mcp.json`. [`examples/signalbot-mcp.json`](../examples/signalbot-mcp.json) is this one, to start from: `sudo install -m 600 examples/signalbot-mcp.json /etc/signalbot-mcp.json`, then `sudoedit /etc/signalbot-mcp.json`.

```json
{"mcpServers": {
  "weather": {
    "url": "https://example.com/mcp",
    "headers": {"Authorization": "Bearer ${WEATHER_TOKEN}"},
    "allowed_tools": ["get_forecast", "get_alerts"],
    "instructions": "Use the weather tools for any forecast, and give temperatures in °C.",
    "keywords": ["weather", "forecast", "rain", "temperature"],
    "chat_commands": {
      "forecast": {"tool": "get_forecast", "require_arg": true, "example": "/forecast Sydney this weekend"},
      "alerts": {"tool": "get_alerts"}
    }
  },
  "geo": {
    "command": "/opt/signalbot-addons/geoguess",
    "chat_commands": {"geo": {"tool": "geo", "direct": true, "example": "/geo 3m", "timeout": "60s"}},
    "addon": {"chats": ["all"], "dms": false, "post": true, "react": true, "watch": true, "watch_max": "1h"}
  },
  "quiz": {
    "command": "/opt/signalbot-addons/quiz",
    "chat_commands": {"quiz": {"tool": "quiz", "direct": true, "example": "/quiz 80s music", "timeout": "90s"}},
    "addon": {"chats": ["all"], "post": true, "react": true, "watch": true, "model": {"per_hour": 30, "max_tokens": 4000}}
  },
  "files": {
    "command": "uvx",
    "args": ["some-mcp-server"],
    "env": {"KEY": "value"},
    "blocked_tools": ["delete_file"],
    "disabled": true
  }
}}
```

| Key | What it does |
|---|---|
| `url`, `headers` | a remote server (Streamable HTTP or SSE), with optional HTTP headers |
| `command`, `args`, `env` | a local server, run with only `PATH`, `HOME` and its `env` |
| `${NAME}` | in `url`, `headers`, `args` or `env`: replaced at startup by the bot's environment variable `NAME` (set it in the unit's `EnvironmentFile`), so a token stays out of this file; one that isn't set stops the bot with an error naming it |
| `allowed_tools` / `blocked_tools` | offer only these tools, or every tool but these (one or the other, not both) |
| `instructions` | a line added to the system prompt while the server's tools are offered (see below) |
| `keywords` | offer the server's tools only when a message has one of these words (see below). A server without them is offered with every message, except one whose page or file the model is shown, so what a document says can't call its tools |
| `chat_commands` | optional slash-command shortcuts to a tool (you can always just ask): `tool` names it, `require_arg` makes a bare command get a usage reply, and `example` adds one to it; for an addon, `direct` has the bot call the tool itself, without the model, and `timeout` sets how long that call may take (see [Addons](#addons)) |
| `addon` | makes a local server an addon, and says where it may act and what it may do (see [Addons](#addons)) |
| `disabled` | leave the server out without deleting it |

**You don't need a command to use the tools: just ask.** For example, `@signalbot what's the weather in Sydney this weekend?` The model sees every tool that's offered and decides when to call one. Without `keywords` a server's tools are always offered; with them, the message needs one of its words, such as `weather` or `forecast` above. Gating tools this way saves tokens.

Two optional fields shape how a server's tools are offered:

- `instructions` is a line added to the system prompt while the server's tools are offered. MCP servers can send their own `instructions` when the bot connects (the MCP spec's hint for the system prompt); without this key those are used, capped at 1,000 characters. Set it to replace them, or to `""` to use none.
- `keywords` offers the server's tools only when a message contains one of the words, ignoring capitals and matching whole words only, and for the `TOOL_LINGER_ROUNDS` replies (default 2) after one that matched or used its tools. Without it, the tools are always offered. A chat command always gets its tool.

`chat_commands` are optional shortcuts on top of asking. `/forecast sydney this weekend` asks the model, which gets that tool on top of whatever else is on and must call it first. With `require_arg`, a bare `/forecast` gets a usage reply from the bot without asking the model: "Put what you want after it, like /forecast <your request>. Try: /forecast Sydney this weekend. Or just ask me in plain words.", the `Try:` from `example`.

A server that can't be reached at startup is logged and left out. A config error stops the bot with the file's path and the server's name: a setting it doesn't know (such as `allowedTools` for `allowed_tools`), with the settings it could be; both `allowed_tools` and `blocked_tools`; or a tool named in either list that the server doesn't have, with the list it was in and the server's tools, so a typo can't leave a blocked tool offered. Keys other MCP clients write and the bot has no use for (`type`, `transport`, `timeout`, `alwaysAllow`, `autoApprove`, `description`) are ignored, so an entry pasted from another client starts. A tool call that takes over 30 seconds tells the model it didn't answer in time, so it can try a narrower request. Local servers get only `PATH`, `HOME` and their `env`, never the bot's own environment.

- **systemd:** keep the file root-only, uncomment the `LoadCredential=` and `Environment=MCP_CONFIG=` lines in `signalbot.service`, and restart. systemd hands the file to the service alone.
- **Docker:** uncomment the MCP volume and the `environment:` lines in `compose.yaml`, then `sudo docker compose up -d`. The image has no shell or runtimes, so only remote servers (`url`) and static binaries, such as an addon (below), work.
- **Local servers under systemd** usually need the hardening loosened with a drop-in (`sudo systemctl edit signalbot`), for example `MemoryDenyWriteExecute=no` for Node, and `CacheDirectory=signalbot` with `Environment=HOME=/var/cache/signalbot` for npx or uvx caches.

## Addons

An addon is a local MCP server that does more than offer tools: it answers its own commands without the model, reads the messages it asks for, and posts and reacts when it likes. [GeoGuess](https://github.com/robdevops/GeoGuess), a photo-guessing game, is one. It is still an ordinary MCP server (protocol 2025-06-18 over stdio), which also speaks signalbot's small extension, under keys starting `io.github.robdevops.signalbot/`. A server is an addon when it has an `addon` block in the MCP file and says it speaks the extension when the bot connects; one with the block that doesn't runs as a plain server, with a warning. Addons run even with [no model](MODELS.md#no-model).

An addon written in Go can use the package [`github.com/robdevops/signalbot/addon`](../addon), which has the extension's types and a ready stdio server: give it the tools, and queue posts, reactions and the end of a watch with `Queue`. [GeoGuess](https://github.com/robdevops/GeoGuess) is built on it. An addon in another language speaks the same JSON, described below and, message by message, in the JSON Schema [`addon/schema.json`](../addon/schema.json), which it can validate what it sends against.

**An addon is trusted code.** It runs as the bot's user, inside the same systemd or Docker sandbox, and could read what that user can. The bot gives it as little as it can (below), but that guards against bugs and leaks, not a hostile program: install only addons you would let run as the bot.

```jsonc
"geo": {
  "command": "/opt/signalbot-addons/geoguess",
  "chat_commands": {"geo": {"tool": "geo", "direct": true, "example": "/geo 3m", "timeout": "60s"}},
  "addon": {
    "chats": ["<group ID>"],
    "dms": false,
    "post": true, "react": true, "watch": true,
    "watch_max": "1h",
    "names": false, "global_ids": false,
    "rate": {"per_minute": 20, "burst": 5}
    // "model": {"per_hour": 30, "max_tokens": 4000}, only for an addon that asks the model
  }
}
```

| Key in `addon` | What it does |
|---|---|
| `chats` | the groups it works in, by ID exactly as Signal or Telegram shows it (a Telegram topic counts as its group), or `["all"]`. A name is an error: names can be changed, IDs can't |
| `dms` | it also works in DMs with people allowed to DM the bot |
| `post` | it may post on its own |
| `react` | it may react to messages |
| `watch` | it may ask to read chosen messages, for a while (below) |
| `watch_max` | the longest a watch may last: 1s to 24h, default 1h |
| `names` | each call it gets carries the sender's display name. Off, it gets none |
| `global_ids` | a person has the same ID in every chat. Off, each chat gives them a different one, so the addon can't follow someone between groups |
| `rate` | its posts and reactions a minute in each chat (`per_minute`, default 20), with bursts of `burst` (default 5) |
| `model` | it may ask the bot's model (below): at most `per_hour` requests an hour, all chats together (default 30), each answer at most `max_tokens` tokens (default 4000, at most 32000) |

Each permission is off unless set. For an addon, `chats`, `dms` and these permissions are the gate: anyone in an allowed chat can use its commands, and nothing it does reaches another chat.

**What an addon sees.** Never a phone number, account ID or group ID: chats and people are pseudonyms the bot makes with a secret kept in `mcp.json` in the state directory, different for each addon. It sees a command's text after the command, the messages its watches match, and, with `names`, display names. To name someone in what it says, it writes their pseudonym as `{u_…}`, and the bot puts in the name it last saw them use there, as an @mention, or "someone".

- **Direct commands.** `"direct": true` on a chat command has the bot call the addon's tool itself, with the text after the command, and send what it answers (text, and up to 2 pictures) without the model. The tool must be marked for the bot, and is then never offered to the model. A call has 15 seconds, or the command's `timeout`, from 1s to 2m (`"timeout": "60s"`), for an addon that may take a while, such as GeoGuess finding a photo; while it runs the chat sees the bot typing. At most 2 run at once per addon; a third is told the addon is busy, so with a long `timeout`, two calls that wait (two chats each waiting for a GeoGuess photo) make even a quick command such as `/geo scores` wait for one to end. When it fails, the chat gets a short line ("GeoGuess couldn't do that just now."), never the addon's own error text. An addon with no tool for the model has its commands named in the prompt's list of what the bot has, so the model can point people to them.
- **Hidden tools.** A tool whose `_meta` says it is for the bot alone (`io.github.robdevops.signalbot/visibility` without `"model"`, or MCP Apps' `ui.visibility` without `"model"`) is never offered to the model or listed as one of its tools, so tools that read chat text stay out of its hands.
- **Watches.** A direct command's answer can ask for a watch on its chat: a pattern, such as map links, until a time at most `watch_max` away. While it lasts, a message there that matches goes to the addon instead: it isn't answered, isn't sent to the model, and isn't kept in history, even as group context; so does an edit of it later. The addon may react to it, so people see it was counted. A pattern that would match ordinary chat ("lol", "see you at 5", a news link, …) is refused, so a watch can't become a feed of everything said; at most 8 run in a chat at once. While a watch lasts, a reply to the command's pictures, or to pictures the addon posts while it lasts, that asks the bot gets the addon's set answer (GeoGuess's "No hints during a round 😉"), not the model's. A reply watch (`"replies": true`) takes instead only replies to the addon's own messages in the chat while it lasts, the command's answer and its posts, that ask the bot (in a DM, every message, as the chat is the player's own with the bot), such as a quiz's free-text answers; its pattern, if it has one, must match them too, and isn't held to the ordinary-chat rule, since such a reply is meant for the addon. A watch with `"reactions": true` also takes reactions put on, or taken off, the command's answer and the addon's posts while it lasts, each a call of its tool with `{"reaction": emoji, "removed": true|false}` and the reactor's context, such as a game's players joining by reacting to its intro. A watch's `over` text answers, for an hour after the watch ends, a message to the bot that replies to one of the addon's messages it knew, in place of the model ("That game is over."), so a late answer isn't played along with. On Telegram the bot sees reactions only where it is an admin.
- **Model.** With `model`, an addon may ask the bot's model for text, through MCP's standard sampling request (`sampling/createMessage`): it sends the prompt, and the bot runs it on its own model, with its own key, and sends back the answer. The addon needs no key or provider of its own. The bot offers this only to an addon with `model`, and only while it has a model; MCP suggests a person approve each request, and here the `model` setting is that approval. Each request names the chat it is for, one the addon may post in now, and is charged to that chat's `DAILY_BUDGET`: one over its budget is refused, and so is one past `per_hour`. A request is text only, at most 16 messages and 32 KB, with one in flight per addon at a time. Only its outcome, time, size, tokens and cost are logged, never the prompt or the answer.
- **Outbox.** An addon posts when it likes by queueing posts (text, up to 2 pictures, or one MP3 clip to play, which shows no title: a voice message on Telegram, `clip.mp3` on Signal), reactions and the end of a watch in its outbox, an MCP resource the bot subscribes to (`signalbot://outbox`), and saying it changed; the bot reads it at once. It may post or react only in a chat where it was used, by a command or a watch, in the last 24 hours, and only within `rate`. Its posts go through the chat's queue like any message, never more than one at a time, and history keeps them as the bot's, labelled with the addon's name (`[GeoGuess] …`), so what an addon writes never poses as a person. A post can be a reply to one of the addon's earlier posts in the same chat, named by that post's ID (`replyTo`), so a quiz can answer a question under it; the Go package's `QueueIDs` gives the IDs. The bot remembers the IDs of the addon's last 256 post ops for 24 hours (a merged post uses one for each op); a reply to one it doesn't know, or in another chat, goes as an ordinary post. A reply is always a message of its own, never merged with the addon's other posts. On Signal it quotes the post; on Telegram it replies to it, and goes anyway if the post was deleted. A reaction can likewise go on one of the addon's own earlier posts, named by its ID (`post`, with its `chat`, in place of `ref`), such as ⏰ on a question when time is nearly up; one on a post the bot doesn't remember sending there, such as one not sent yet, is dropped.
- **Settings.** An addon has no `env` block. It gets `PATH`, `HOME`, the bot's time zone (`TZ`) and proxy settings (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`, and `SSL_CERT_FILE` or `SSL_CERT_DIR` for a custom CA) if set, and every variable in the bot's environment (`/etc/signalbot.env`) that starts with its server's name in capitals, anything but letters and digits as `_`, then `_`: the server `geo` gets `GEO_STATE_DIR`, `GEO_MAPILLARY_TOKEN` and so on, and never the bot's own keys. A name whose prefix starts one of the bot's settings (such as `model`, `signal` or `mcp`) is a config error. An addon that keeps files takes its directory from `<PREFIX>STATE_DIR` (`GEO_STATE_DIR`) and creates it itself, mode 0700. Unless the bot's environment sets it, the bot sets it to `addons/<server name>` in the bot's state directory (`/var/lib/private/signalbot/addons/geo`, or `/data/addons/geo` in Docker), so an addon's files are kept with the bot's; it creates nothing there itself. With `STATE_DIRECTORY` unset, or a server name that isn't only letters, digits, `.`, `_` and `-` (up to 64, not starting with `.`), there is no default, and the addon falls back on its own.
- **Logs.** The bot logs an addon's commands, calls and drops by name, outcome, time and size, never what was said. An addon's stderr is counted ("addon geo wrote 3 lines (210 bytes) to stderr", at most once a minute and once more when it exits), never logged, since it may echo a message. To debug an addon, run it on its own with stderr going to your own file.
- **State.** The bot keeps an addon's watches, the messages they took, the chats it may post in, how far it has read its outbox and the posts a later one may reply to in `mcp.json`, so a restart of either doesn't lose a round. See [PRIVACY.md](../PRIVACY.md#addons).
- **Docker.** The image has no shell or runtimes, so an addon must be a static binary (`CGO_ENABLED=0 go build`), mounted read-only at the same path, such as `- /opt/signalbot-addons:/opt/signalbot-addons:ro` in `compose.yaml`, so its `command` is the same under Docker and systemd (whose `ProtectSystem=strict` leaves `/opt` readable). The state directory must be writable by user 65532 for `mcp.json`, and so must any directory the addon keeps files in. Addons share the container's or unit's memory and process limits, so raise `mem_limit` or `MemoryMax` to suit; `GOMEMLIMIT` isn't passed on, so a Go addon takes its limit from its own setting (GeoGuess: `GEO_MEMLIMIT`).

---

[Back to README.md](../README.md)
