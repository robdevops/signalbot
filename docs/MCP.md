# MCP tools

The model can call tools from [MCP](https://modelcontextprotocol.io) servers, remote (`url`) or local (`command`). Anyone who can address the bot can make it call them, so choose what each server offers: `allowed_tools` offers only the tools listed, `blocked_tools` offers all but those, and without either every tool the server has is offered. Put the servers in a file, for example `/etc/signalbot-mcp.json`:

```json
{"mcpServers": {
  "weather": {
    "url": "https://example.com/mcp",
    "headers": {"Authorization": "Bearer …"},
    "allowed_tools": ["get_forecast", "get_alerts"],
    "instructions": "Use the weather tools for any forecast, and give temperatures in °C.",
    "keywords": ["weather", "forecast", "rain", "temperature"],
    "chat_commands": {
      "forecast": {"tool": "get_forecast", "require_arg": true},
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
| `allowed_tools` / `blocked_tools` | offer only these tools, or every tool but these |
| `instructions` | a line added to the system prompt while the server's tools are offered (see below) |
| `keywords` | offer the server's tools only when a message has one of these words (see below). A server without them is offered with every message, except one whose page or file the model is shown, so what a document says can't call its tools |
| `chat_commands` | optional slash-command shortcuts to a tool (you can always just ask): `tool` names it, `require_arg` makes a bare command get a usage reply |
| `disabled` | leave the server out without deleting it |

**You don't need a command to use the tools: just ask.** For example, `@signalbot what's the weather in Sydney this weekend?` The model sees every tool that's offered and decides when to call one. Without `keywords` a server's tools are always offered; with them, the message needs one of its words, such as `weather` or `forecast` above. Gating tools this way saves tokens.

Two optional fields shape how a server's tools are offered:

- `instructions` is a line added to the system prompt while the server's tools are offered. MCP servers can send their own `instructions` when the bot connects (the MCP spec's hint for the system prompt); without this key those are used, capped at 1,000 characters. Set it to replace them, or to `""` to use none.
- `keywords` offers the server's tools only when a message contains one of the words, ignoring capitals and matching whole words only, and for the `TOOL_LINGER_ROUNDS` replies (default 2) after one that matched or used its tools. Without it, the tools are always offered. A chat command always gets its tool.

`chat_commands` are optional shortcuts on top of asking. `/forecast sydney this weekend` asks the model, which gets that tool on top of whatever else is on and must call it first. With `require_arg`, a bare `/forecast` gets a usage reply from the bot without asking the model.

A server that can't be reached at startup is logged and left out. A tool named in either list that the server doesn't have is a config error, so a typo can't leave a blocked tool offered. Local servers get only `PATH`, `HOME` and their `env`, never the bot's own environment.

- **systemd:** keep the file root-only, uncomment the `LoadCredential=` and `Environment=MCP_CONFIG=` lines in `signalbot.service`, and restart. systemd hands the file to the service alone.
- **Docker:** add `-v /etc/signalbot-mcp.json:/etc/signalbot-mcp.json:ro -e MCP_CONFIG=/etc/signalbot-mcp.json`. The image has no shell or runtimes, so only remote servers (`url`) work.
- **Local servers under systemd** usually need the hardening loosened with a drop-in (`sudo systemctl edit signalbot`), for example `MemoryDenyWriteExecute=no` for Node, and `CacheDirectory=signalbot` with `Environment=HOME=/var/cache/signalbot` for npx or uvx caches.
