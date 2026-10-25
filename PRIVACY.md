# Privacy

What happens to your messages when this bot is in a chat. For how the bot is built, see [TECHNICAL.md](TECHNICAL.md).

## What leaves your machine

When the bot answers a message, it sends this to the AI provider you configured ([OpenRouter](https://openrouter.ai) or [x.ai](https://x.ai)):

- its instructions (the built-in prompt, plus `SYSTEM_PROMPT` and `INTRO`);
- the earlier messages in this chat that were addressed to the bot, with the bot's replies: at most `HISTORY_TURNS` exchanges, none older than a day. Other conversation in the group is never included, apart from a message quoted as described below;
- the new message, with the display name of whoever sent it (in groups) and of anyone it @mentions;
- the quoted text, when someone replies to one of the bot's messages, or mentions the bot in reply to someone else.

With web search on (`MODEL_SEARCH`), the model may also search the web. The search query comes from the conversation and is handled by the provider's search service.

Every request asks the provider not to keep it: x.ai gets `store: false`, and OpenRouter is told to use only providers that do not collect the data (`data_collection: deny`). These are requests. The bot cannot check or enforce what the provider does, so read the provider's own policy.

Messages that are not addressed to the bot are never sent anywhere, not even to the bot's own logs. The one exception is a DM from someone who is not in `SIGNAL_DM_USER_IDS`: the log records their id (never their name or what they wrote), for convenience, so that you can add the ID to `SIGNAL_DM_USER_IDS` if desired.

## What the bot keeps

- **Conversation history is kept in memory only.** It is per chat, holds at most `HISTORY_TURNS` exchanges (and 500 chats), and nothing is kept longer than 24 hours. If the chat has a shorter disappearing-messages timer, that timer applies, and the messages are removed from memory when it runs out. A restart clears everything. The bot never writes history to disk.
- **No swap or core dumps.** The included systemd unit and Docker command forbid swapping the bot's memory and writing core dumps, so history cannot reach disk that way.
- **Memory is freed, not wiped.** When a message expires, the bot drops it and Go's garbage collector reclaims the memory, but the bytes may remain until something overwrites them. Someone with root access to the host could read them from the running process.
- **Names.** The bot remembers display names (up to 5,000) so it can show and mention people by name. It learns them from signal-cli's contact list and from group members who write in a group the bot is in, including when they are not talking to the bot. Only the name and id are kept, never what they wrote. Names stay in memory until a restart, or until the cache is full and older ones are pushed out.

## What the bot logs

- **A message addressed to the bot:** a line saying what kind it was (mention, reply or DM) and who sent it, by name and id. After the model answers, a line with the sender's name, how long it took, the tokens used and the cost.
- **Never the text of any message.** Errors are logged without quoting messages, and API errors without the request.
- **A message not addressed to the bot:** nothing, not even a note that it arrived.
- **A DM from someone not in `SIGNAL_DM_USER_IDS`:** their id only, with no name and no text.

The logs go to the journal or to Docker's log driver, which keep them according to your settings. Names and ids stay there as long as the logs do.

## What the bot can see

In a group, signal-cli passes every message to the bot, and the bot looks at each one to decide whether it is addressed to it. Messages that are not addressed to it are dropped on the spot; apart from the sender's name and id (see above), nothing about them is kept. In DMs, the bot answers only the ids listed in `SIGNAL_DM_USER_IDS`.

## What this does not cover

- **The provider.** Once a message is sent, the provider's retention, logging and sub-processors apply.
- **End-to-end encryption.** Signal's encryption protects messages between people. The bot is a participant: signal-cli decrypts what the bot receives, and what the bot answers is sent to the provider. People in a chat should know the bot is there.
- **signal-cli's own data.** The Signal account keys and whatever signal-cli stores are in signal-cli's data directory, which the bot does not read. Keep that directory private (see the [README](README.md#prerequisites)).
- **Anyone in the `signal-cli` group** can use the socket to drive the Signal account, so keep the group to the bot.
- **The host.** Root users can read process memory, and backups of the logs keep what the logs contain.
