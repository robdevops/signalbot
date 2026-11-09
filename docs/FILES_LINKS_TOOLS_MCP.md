# Files, Links, Tools, and MCP connectors

- [Files](#files)
- [Links](#links)
- [How tools are offered](#how-tools-are-offered)
- [MCP connectors](#mcp-connectors)

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

## How tools are offered

Web search and tools without `keywords` are offered with every message, except that on a message whose page or file the model is shown (its text, a PDF or audio), tools without `keywords` are left out (web search and a chat command's own tool stay), so text inside a document can't call a tool: only what someone typed can. Charts, and MCP servers with `keywords`, are offered only when a message matches their words, then for `TOOL_LINGER_ROUNDS` more replies, and each reply that matches or uses one of their tools starts that count again. So "chart NVDA", "add AMD", "make it a year" keeps the charts on however long it runs; only replies that leave them unused count down.

The default of 2 is meant to cover a short aside ("thanks!", "what's AMD's ticker again?") before you come back to the chart, without the tools still being on when the conversation has moved on. Lingering longer has two costs: a tool on for unrelated messages is one the model may use when it shouldn't (a stock tool picked for a population question), and its descriptions add to every request. The prompt cache gains little, because the offered tools change once either way when a group drops off. Raise it if people often come back to a chart after several other questions; set `0` to offer a group only on messages that name it.

## MCP connectors

The model can call tools from [MCP](https://modelcontextprotocol.io) servers, remote (`url`) or local (`command`). Anyone who can address the bot can make it call them, so choose what each server offers: `allowed_tools` offers only the tools listed, `blocked_tools` offers all but those, and without either every tool the server has is offered. Put the servers in a file, for example `/etc/signalbot-mcp.json`. [`examples/signalbot-mcp.json`](../examples/signalbot-mcp.json) is this one, to start from: `sudo install -m 600 examples/signalbot-mcp.json /etc/signalbot-mcp.json`, then `sudoedit /etc/signalbot-mcp.json`.

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
| `chat_commands` | optional slash-command shortcuts to a tool (you can always just ask): `tool` names it, `require_arg` makes a bare command get a usage reply, and `example` adds one to it |
| `disabled` | leave the server out without deleting it |

**You don't need a command to use the tools: just ask.** For example, `@signalbot what's the weather in Sydney this weekend?` The model sees every tool that's offered and decides when to call one. Without `keywords` a server's tools are always offered; with them, the message needs one of its words, such as `weather` or `forecast` above. Gating tools this way saves tokens.

Two optional fields shape how a server's tools are offered:

- `instructions` is a line added to the system prompt while the server's tools are offered. MCP servers can send their own `instructions` when the bot connects (the MCP spec's hint for the system prompt); without this key those are used, capped at 1,000 characters. Set it to replace them, or to `""` to use none.
- `keywords` offers the server's tools only when a message contains one of the words, ignoring capitals and matching whole words only, and for the `TOOL_LINGER_ROUNDS` replies (default 2) after one that matched or used its tools. Without it, the tools are always offered. A chat command always gets its tool.

`chat_commands` are optional shortcuts on top of asking. `/forecast sydney this weekend` asks the model, which gets that tool on top of whatever else is on and must call it first. With `require_arg`, a bare `/forecast` gets a usage reply from the bot without asking the model: "Put what you want after it, like /forecast <your request>. Try: /forecast Sydney this weekend. Or just ask me in plain words.", the `Try:` from `example`.

A server that can't be reached at startup is logged and left out. A config error stops the bot with the file's path and the server's name: a setting it doesn't know (such as `allowedTools` for `allowed_tools`), with the settings it could be; both `allowed_tools` and `blocked_tools`; or a tool named in either list that the server doesn't have, with the list it was in and the server's tools, so a typo can't leave a blocked tool offered. Keys other MCP clients write and the bot has no use for (`type`, `transport`, `timeout`, `alwaysAllow`, `autoApprove`, `description`) are ignored, so an entry pasted from another client starts. A tool call that takes over 30 seconds tells the model it didn't answer in time, so it can try a narrower request. Local servers get only `PATH`, `HOME` and their `env`, never the bot's own environment.

- **systemd:** keep the file root-only, uncomment the `LoadCredential=` and `Environment=MCP_CONFIG=` lines in `signalbot.service`, and restart. systemd hands the file to the service alone.
- **Docker:** uncomment the MCP volume and the `environment:` lines in `compose.yaml`, then `sudo docker compose up -d`. The image has no shell or runtimes, so only remote servers (`url`) work.
- **Local servers under systemd** usually need the hardening loosened with a drop-in (`sudo systemctl edit signalbot`), for example `MemoryDenyWriteExecute=no` for Node, and `CacheDirectory=signalbot` with `Environment=HOME=/var/cache/signalbot` for npx or uvx caches.

---

[Back to README.md](../README.md)
