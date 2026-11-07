# Configuration

Every setting, read from the environment (`/etc/signalbot.env` in the install). Only `MODEL_API_KEY` and `SIGNAL_DM_USER_IDS` are needed to get started, and `SIGNALCLI_SOCKET` if signal-cli's socket isn't at `/run/signal-cli/socket`; see the [README](../README.md#configuration).

| Variable | Default | Description |
|---|---|---|
| `SIGNALCLI_SOCKET` | `/run/signal-cli/socket` | Unix socket path, or `host:port` for TCP |
| `SIGNAL_DM_USER_IDS` | empty | UUIDs/numbers allowed to DM the bot, separated by commas or whitespace; empty turns DMs off, and the startup log says which |
| `MODEL` | `deepseek/deepseek-v4.1-flash-1017` | The model's name at the API: on OpenRouter as `provider/model` |
| `MODEL_API_KEY` | empty | API key for the model's API, such as your [OpenRouter](https://openrouter.ai) key; none is sent when empty. With neither this nor `MODEL_API_URL` the bot has [no model](MODELS.md#no-model) |
| `MODEL_API_URL` | `https://openrouter.ai/api/v1` | Base URL of an OpenAI-compatible chat completions API; see [Models](MODELS.md#other-providers) |
| `MODEL_REASONING` | `low` | Reasoning effort, e.g. `low`, `medium`, `high` (`none` to omit). Sent as OpenRouter's `reasoning.effort`, or elsewhere as `reasoning_effort` |
| `MODEL_SEARCH` | `true` | Lets the model search the web (OpenRouter's web search tool) when it decides to, citing the sites it uses by url; searches are billed. Only on OpenRouter: on another API it is left out, with a warning. Accepts `true`/`false`, `yes`/`no`, `on`/`off`, `1`/`0` |
| `MODEL_SYSTEM_PROMPT` | empty | Extra instructions, appended to the built-in prompt (which keeps answers short and Signal-sized and, when the model has tools, asks it to name its source) |
| `DAILY_BUDGET` | `1` | US dollars each group and each DM may spend on the model a day, midnight to midnight in `TZ`; `0` for no cap. Once a chat reaches it, the bot says once "I've reached today's spending limit for this chat, so I'll answer again in 5 hours. Commands that don't need me to think still work." and until midnight reacts ⏳ to anything else there, answering only commands that don't use the model. Today's spending is saved in `budget.json` in `STATE_DIRECTORY`, so a restart doesn't reset it. Only OpenRouter reports costs, so with another API it isn't applied |
| `DARKMODE` | `off` | Chart colours: `off` for light charts (also `false`, `no`, `0`, `light`), `on` for dark (also `true`, `yes`, `1`, `dark`), or the hours charts are dark in `TZ`, such as `18:00-6:00`, `18-6` or `6pm-6am` |
| `GROUP_CONTEXT` | `true` | Remember group messages not addressed to the bot as context, so "what do you think @bot" works without replying to the message; they are never logged |
| `HISTORY_MAX` | `10` | Most messages remembered per chat, the bot's replies and, in groups, messages not addressed to it included. When full, the older half is dropped, so the start of the model's request stays the same for several messages and comes from the API's prompt cache (cheaper and faster); below 3, the oldest message is dropped |
| `INTRO` | built-in sentence | Overrides the self-introduction; `{name}`, `{model}` (without any OpenRouter `provider/` prefix) and `{tools}` (the names of the tools, or `none`) are substituted |
| `MAX_CONCURRENCY` | `32` | Conversations in flight at once; messages beyond this are dropped, with a warning |
| `MCP_CONFIG` | empty | Path of an MCP servers file; see [MCP connectors](FILES_LINKS_TOOLS_MCP.md#mcp-connectors) |
| `STATE_DIRECTORY` | working directory | Where features save their state, one file each, such as `/schedule`'s `schedules.json` (and the core its `budget.json`); the bot won't start if a feature that saves can't list and write it. Set by systemd's `StateDirectory=signalbot` (`/var/lib/private/signalbot`) and by the Docker image (`/data`, which must be writable by user 65532) |
| `TOOL_LINGER_ROUNDS` | `2` | Replies a keyword-gated tool group (charts, an MCP server with `keywords`) stays offered after the last one that matched its keywords or used its tools; `0` offers it only on a matching message. Any use or match starts the count again, so a run of follow-ups keeps it. Capped at `HISTORY_MAX`. See [How tools are offered](FILES_LINKS_TOOLS_MCP.md#how-tools-are-offered) |
| `TZ` | `UTC` in Docker, the host's under systemd | Time zone for `/schedule` times without an offset and for `DARKMODE` hours, e.g. `Australia/Sydney` |

A number that can't be read is warned about at startup, and the default used. `OPENROUTER_API_KEY` is now `MODEL_API_KEY`, and `REASONING` and `SYSTEM_PROMPT` are now `MODEL_REASONING` and `MODEL_SYSTEM_PROMPT`; the old names are ignored, with a warning at startup. `LOG_LEVEL` is gone: every level is always written (see [Logs](../TECHNICAL.md#logs)).

The default `INTRO` is short paragraphs: `hi, I'm *{name}*, your open source assistant running {model}.`, `I respond to @{name} and replies to my messages.`, `Tools:` then `{tools}`, one tool category per paragraph, then `{more}`, lines features add (such as `Type "/schedule" for schedules and reminders.`, only in a DM, where schedules are made), then the link `https://github.com/robdevops/signalbot/blob/main/README.md` (Signal makes a bare url clickable; it has no named links).

The cost logged with each reply is the amount OpenRouter reports billing for it. An API that reports no cost gets none logged.

---

[Back to README.md](../README.md)
