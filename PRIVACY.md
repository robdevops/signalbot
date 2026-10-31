# Privacy

What happens to your messages when this bot is in a chat. For how the bot is built, see [TECHNICAL.md](TECHNICAL.md).

## What leaves your machine

When the bot answers a message, it sends this to [OpenRouter](https://openrouter.ai), which passes it to the model's provider:

- its instructions (the built-in prompt, plus `MODEL_SYSTEM_PROMPT`, `INTRO` and any MCP server's `prompt`);
- the chat's recent messages, with the bot's replies: at most `HISTORY_TURNS` × 2 messages, none older than a day. In a group this is the whole conversation, including messages not addressed to the bot, so it can answer "what do you think?" about something just said; set `GROUP_CONTEXT=false` to keep only the messages addressed to it;
- the new message, with the display name of whoever sent it (in groups) and of anyone it @mentions;
- the quoted text, when someone replies to one of the bot's messages, or mentions the bot in reply to someone else.
- photos or a sticker in the message (a sticker also by its emoji and pack title), and the thumbnail of a quoted photo, when the model accepts images (the bot checks this at startup).

With web search on (`MODEL_SEARCH`), the model decides whether to search and writes the query itself. The query can contain anything the model was sent (see above), such as a name or a phrase from a message, and goes to the search service OpenRouter uses.

Charts are drawn on the bot's own machine from the numbers the model passes in; nothing extra leaves it.

With MCP tools configured, the model may call them. A call sends the arguments the model wrote, which can quote anything above, to that tool's MCP server, and the tool's result goes to OpenRouter with the next request.

Every request tells OpenRouter to use only model providers that do not collect data (`data_collection: deny`). That is a request the bot cannot check or enforce, so read OpenRouter's policy and the model provider's.

## What the bot keeps

- **History lives in memory only.** It is kept per chat, up to `HISTORY_TURNS` exchanges and 500 chats, for at most 24 hours, or until a shorter disappearing-messages timer runs out. A restart clears it, and the bot never writes it to disk. In groups it holds messages not addressed to the bot too (their text only), unless `GROUP_CONTEXT=false`. Photos are not kept at all: the history notes that one was sent, and the bot fetches a photo from signal-cli only while answering a message that includes it.
- **No swap or core dumps.** The included systemd unit and Docker command forbid both, so history cannot reach disk that way.
- **Memory is freed, not wiped.** An expired message is dropped and Go's garbage collector reclaims its memory, but the bytes may linger until overwritten, and someone with root access to the host could read them from the running process.
- **Schedules are kept on disk.** Each `/schedule` saves, in `schedules.json` in the state directory (`/var/lib/private/signalbot`), the scheduling user's ID, the group's ID and its name when scheduled (if it's for a group), the time and repeat, and the text to post, until the user deletes it with `/schedule del`. Nothing else the bot keeps is written to disk.
- **Names.** The bot keeps a copy of signal-cli's contact list (names and IDs) in memory, so it can show and mention people by name. It reloads the list at startup, after reconnecting, when a group changes, and at least daily when in use, replacing what it had, so people who leave a group are forgotten. It also keeps the name of anyone who sends it an addressed message. It never keeps what anyone wrote.

## What the bot logs

Every level, debug included, is always written; there is no setting to turn any of it down.

- **A message addressed to the bot:** "received group chat" or "received dm", with no name, ID or text; after the answer, a line with the sender's name, time taken, tokens used and cost.
- **Never the text of any message,** nor errors that quote one.
- **A message not addressed to the bot:** nothing, not even a note that it arrived.
- **A DM from someone not in `SIGNAL_DM_USER_IDS`:** their ID, every time, never a name or text. The same goes for a scheduled run in the DM of a user since removed from the list.
- **A tool call:** the tool's name and how long it took; never the arguments or the result.
- **Schedules:** how many were loaded at startup, a run skipped or dropped, and how long each run took; never the text or who made it. A run dropped because its user left the group is logged without content.
- **Debug lines:** the number of contacts loaded, the ID of a mentioned person with no known name, the name of a sender whose repeated message was dropped, and a local MCP server's own error output.

Logs go to the journal or Docker's log driver and are kept as long as your settings keep them, names and IDs included.

## What the bot can see

In a group, signal-cli passes every message to the bot, which checks each one for whether it is addressed to it. Messages not addressed to it are never logged and never answered. Their text, with the sender's display name, is kept in that group's history as context, the same way and for the same time as everything else there, unless `GROUP_CONTEXT=false`, in which case they are dropped on the spot. In DMs, the bot answers only the IDs listed in `SIGNAL_DM_USER_IDS`.

## What this does not cover

- **The provider.** Once a message is sent, the model provider's retention, logging and sub-processors apply.
- **End-to-end encryption.** Signal's encryption protects messages between people. The bot is a participant: signal-cli decrypts what it receives, and what it answers goes to the provider. People in a chat should know the bot is there.
- **signal-cli's own data.** The account keys and whatever else signal-cli stores sit in its data directory, which the bot does not read. Keep it private (see the [README](README.md#prerequisites)).
- **Anyone in the host's `signal-cli` Unix group** (not a Signal group) can use the socket to drive the Signal account, so keep that group's members to just the bot code.
- **The host.** Root users can read process memory, and log backups keep what the logs contain.
