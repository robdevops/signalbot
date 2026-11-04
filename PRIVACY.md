# Privacy

What happens to your messages when this bot is in a chat. For how the bot is built, see [TECHNICAL.md](TECHNICAL.md).

## What leaves your machine

When the bot answers a message, it sends this to the model's API: [OpenRouter](https://openrouter.ai) by default, which passes it to the model's provider, or the API you set in `MODEL_API_URL` (see [other providers](docs/PROVIDERS.md)). With [no model](docs/NO_MODEL.md), nothing is sent anywhere:

- its instructions (the built-in prompt, plus `MODEL_SYSTEM_PROMPT`, `INTRO` and any MCP server's `instructions`);
- the chat's recent messages, with the bot's replies: at most `HISTORY_MAX` messages (the older half dropped when full), none older than a day. In a group this is the whole conversation, including messages not addressed to the bot, so it can answer "what do you think?" about something just said; set `GROUP_CONTEXT=false` to keep only the messages addressed to it;
- the new message, with the display name of whoever sent it (in groups) and of anyone it @mentions;
- the quoted text, when someone replies to one of the bot's messages, or mentions the bot in reply to someone else.
- files in the message, or in the message it replies to: a PDF or audio as itself (on OpenRouter, a PDF for a model that can't read files is read by OpenRouter's OCR service, `mistral-ocr`), and text, Word, Excel and PowerPoint files as their text, for that one request. The bot never keeps a file, and history notes only that it was attached;
- photos or a sticker in the message (a sticker also by its emoji and pack title), and the thumbnail of a quoted photo, when the model accepts images (on OpenRouter the bot checks its model list at startup; with another provider it sends them until the API rejects one).

With web search on (`MODEL_SEARCH`), the model decides whether to search and writes the query itself. The query can contain anything the model was sent (see above), such as a name or a phrase from a message, and goes to the search service OpenRouter uses. Web search runs only on OpenRouter.

When a message that addresses the bot has a link, or replies to one with a link, the bot fetches the page from its own machine, so the site sees that machine's address and the user agent `signalbot (+https://github.com/robdevops/signalbot)`, never who sent the link. The page's text then goes to the model's API with the message. The bot never fetches a link the model or a page suggests, never a private or local address, and never a link that looks private or one-time (a reset, confirmation, unsubscribe or webhook link, or one carrying a token, key or signature), so it can't spend one. To cap links at 20 an hour per chat it counts, in memory, when each chat last had links fetched, never which.

Charts are drawn on the bot's own machine from the numbers the model passes in; nothing extra leaves it.

With MCP tools configured, the model may call them. A call sends the arguments the model wrote, which can quote anything above, to that tool's MCP server, and the tool's result goes to the model's API with the next request.

Every request to OpenRouter tells it to use only model providers that do not collect data (`data_collection: deny`). That is a request the bot cannot check or enforce, so read OpenRouter's policy and the model provider's. Other APIs get no such request, since it is OpenRouter's own; read their data policy. A local server, such as Ollama on the same machine, keeps everything on it.

## What the bot keeps

- **With no model, no history at all.** The bot keeps no messages, not even group context, since nothing would read them.
- **History lives in memory only.** It is kept per chat, up to `HISTORY_MAX` messages and 500 chats, for at most 24 hours, or until a shorter disappearing-messages timer runs out. A restart clears it, and the bot never writes it to disk. In groups it holds messages not addressed to the bot too (their text only), unless `GROUP_CONTEXT=false`. Photos are not kept at all: the history notes that one was sent, and the bot fetches a photo from signal-cli only while answering a message that includes it.
- **No swap or core dumps.** The included systemd unit and Docker command forbid both, so history cannot reach disk that way.
- **Memory is freed, not wiped.** An expired message is dropped and Go's garbage collector reclaims its memory, but the bytes may linger until overwritten, and someone with root access to the host could read them from the running process.
- **Schedules are kept on disk.** Each `/schedule` saves, in `schedules.json` in the state directory (`/var/lib/private/signalbot`), the scheduling user's ID, the group's ID and its name when scheduled (if it's for a group), the time and repeat, and the text to post, until the user deletes it with `/schedule del`. The file also keeps, for each user who has scheduled, the last schedule number they were given, so numbers are never reused; it stays after their schedules are deleted. Besides today's spending (below), nothing else the bot keeps is written to disk.
- **Today's spending, on disk.** With a daily budget (`DAILY_BUDGET`, on by default), `budget.json` in the state directory holds today's date and, for each chat that used the model today, what it spent and whether it was told it reached the limit. Chats are stored under a SHA-256 hash of their ID, so the file holds no phone numbers, account IDs or group IDs as such. A hash of a phone number could be matched by trying every number, so treat the file as you would the schedules. Each day's file replaces the last.
- **Recent attachments, in memory only.** So that "@bot summarise this" in reply to someone's file can fetch the original, the bot remembers, for each group message and each DM from an allowed sender with attachments, the attachments' IDs, file names, types and sizes, never their content, under the chat, sender and time. Each is forgotten after a day, or sooner when 2,000 newer messages are remembered, and a restart clears them all.
- **Names.** The bot keeps a copy of signal-cli's contact list (names and IDs) in memory, so it can show and mention people by name. It reloads the list at startup, after reconnecting, when a group changes, and at least daily when in use, replacing what it had, so people who leave a group are forgotten. It also keeps the name of anyone who sends it an addressed message. It never keeps what anyone wrote.

## What the bot logs

Every level, debug included, is always written; there is no setting to turn any of it down.

- **A message addressed to the bot:** "received group chat" or "received dm", with no name, ID or text; after the answer, a line with the time taken, tokens used and cost (when the API reports it), again with no name or ID, or "reacting instead of replying" when the answer is a reaction.
- **Never the text of any message,** nor errors that quote one.
- **A message not addressed to the bot:** nothing, not even a note that it arrived.
- **A DM from someone not in `SIGNAL_DM_USER_IDS`:** their ID, every time, never a name or text. The same goes for a scheduled run in the DM of a user since removed from the list.
- **A tool call:** the tool's name and how long it took; never the arguments or the result.
- **A link, file, photo or sticker in a message addressed to the bot:** one line each with what the bot did with it and, once it was fetched, its size (how much text was read, a PDF or audio attached, an image shrunk, or why it failed); one line when a link wasn't fetched (it looked private or one-time, the chat's 20 an hour were used, or it was past the second in a message) or when the model can't see the images or sticker; never the link, not even its host, the file's name or the type it was sent as (a file the bot can't read is logged with the type sniffed from its content), the page's title, a sticker's emoji or pack title, or any text.
- **Schedules:** how many were loaded at startup, a run skipped or dropped, and how long each run took; never the text or who made it. A run dropped because its user left the group is logged without content.
- **Names and IDs:** never anyone's name, and never anyone's ID except as above for a DM from someone not in `SIGNAL_DM_USER_IDS`. The bot logs its own account ID at startup.
- **Debug lines:** the number of contacts loaded, that a repeated message was dropped, and a local MCP server's own error output, which the bot doesn't control, so each line has every name and ID the bot knows replaced with "redacted" ("Alice's" becomes "redacted's"), along with any UUID or phone number, before it is written.

Logs go to the journal or Docker's log driver and are kept as long as your settings keep them.

## What the bot can see

In a group, signal-cli passes every message to the bot, which checks each one for whether it is addressed to it. Messages not addressed to it are never logged and never answered. Their text, with the sender's display name, is kept in that group's history as context, the same way and for the same time as everything else there, unless `GROUP_CONTEXT=false`, in which case they are dropped on the spot. In DMs, the bot answers only the IDs listed in `SIGNAL_DM_USER_IDS`.

## What this does not cover

- **The provider.** Once a message is sent, the model provider's retention, logging and sub-processors apply.
- **End-to-end encryption.** Signal's encryption protects messages between people. The bot is a participant: signal-cli decrypts what it receives, and what it answers goes to the provider. People in a chat should know the bot is there.
- **signal-cli's own data.** The account keys and whatever else signal-cli stores sit in its data directory, which the bot does not read. Keep it private (see the [README](README.md#prerequisites)).
- **Anyone in the host's `signal-cli` Unix group** (not a Signal group) can use the socket to drive the Signal account, so keep that group's members to just the bot code.
- **The host.** Root users can read process memory, and log backups keep what the logs contain.
