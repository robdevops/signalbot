# Privacy

What happens to your messages when this bot is in a chat. For how the bot is built, see [TECHNICAL.md](TECHNICAL.md).

## What leaves your machine

When the bot answers a message, it sends this to [OpenRouter](https://openrouter.ai), which passes it to the model's provider:

- its instructions (the built-in prompt, plus `SYSTEM_PROMPT` and `INTRO`);
- the earlier messages in this chat that were addressed to the bot, with the bot's replies: at most `HISTORY_TURNS` exchanges, none older than a day. Other conversation in the group is never included, apart from a message quoted as described below;
- the new message, with the display name of whoever sent it (in groups) and of anyone it @mentions;
- the quoted text, when someone replies to one of the bot's messages, or mentions the bot in reply to someone else.
- photos or a sticker in the message, and the thumbnail of a quoted photo, when the model accepts images (the bot checks this at startup).

With web search on (`MODEL_SEARCH`), the model decides whether to search and writes the query itself. The query can contain anything the model was sent (see above), such as a name or a phrase from a message, and goes to the search service OpenRouter uses.

Charts are drawn on the bot's own machine from the numbers the model passes in; nothing extra leaves it.

With MCP tools configured, the model may call them. A call sends the arguments the model wrote, which can quote anything above, to that tool's MCP server, and the tool's result goes to OpenRouter with the next request.

Every request tells OpenRouter to use only model providers that do not collect data (`data_collection: deny`). That is a request the bot cannot check or enforce, so read OpenRouter's policy and the model provider's.

## What the bot keeps

- **History lives in memory only.** It is kept per chat, up to `HISTORY_TURNS` exchanges and 500 chats, for at most 24 hours, or until a shorter disappearing-messages timer runs out. A restart clears it, and the bot never writes it to disk. Photos are not kept at all: the history notes that one was sent, and the bot fetches it from signal-cli only while answering.
- **No swap or core dumps.** The included systemd unit and Docker command forbid both, so history cannot reach disk that way.
- **Memory is freed, not wiped.** An expired message is dropped and Go's garbage collector reclaims its memory, but the bytes may linger until overwritten, and someone with root access to the host could read them from the running process.
- **Names.** The bot keeps a copy of signal-cli's contact list (names and IDs) in memory, so it can show and mention people by name. It reloads the list at startup, after reconnecting, when a group changes, and at least daily when in use, replacing what it had, so people who leave a group are forgotten. It also keeps the name of anyone who sends it an addressed message. It never keeps what anyone wrote.

## What the bot logs

- **A message addressed to the bot:** a line with the kind (mention, reply or DM) and the sender's name and ID; after the answer, a line with the sender's name, time taken, tokens used and cost.
- **Never the text of any message,** nor errors that quote one.
- **A message not addressed to the bot:** nothing, not even a note that it arrived.
- **A message with photos or a sticker:** how many, never the images or their names.
- **An MCP tool call:** the tool's name, its server and how long it took; never the arguments or the result.
- **A contact list load,** at `LOG_LEVEL=debug` only: a line with the number of contacts.
- **A DM from someone not in `SIGNAL_DM_USER_IDS`:** their ID only, never a name or text.

Logs go to the journal or Docker's log driver and are kept as long as your settings keep them, names and IDs included.

## What the bot can see

In a group, signal-cli passes every message to the bot, which checks each one for whether it is addressed to it. Unaddressed messages are dropped on the spot, and nothing about them is kept. In DMs, the bot answers only the IDs listed in `SIGNAL_DM_USER_IDS`.

## What this does not cover

- **The provider.** Once a message is sent, the model provider's retention, logging and sub-processors apply.
- **End-to-end encryption.** Signal's encryption protects messages between people. The bot is a participant: signal-cli decrypts what it receives, and what it answers goes to the provider. People in a chat should know the bot is there.
- **signal-cli's own data.** The account keys and whatever else signal-cli stores sit in its data directory, which the bot does not read. Keep it private (see the [README](README.md#prerequisites)).
- **Anyone in the host's `signal-cli` Unix group** (not a Signal group) can use the socket to drive the Signal account, so keep that group's members to just the bot code.
- **The host.** Root users can read process memory, and log backups keep what the logs contain.
