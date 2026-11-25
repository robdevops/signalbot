# Privacy

What happens to your messages when this bot is in a chat. For how the bot is built, see [TECHNICAL.md](TECHNICAL.md).

## What leaves your machine

When the bot answers a message, it sends this to the model's API: [OpenRouter](https://openrouter.ai) by default, which passes it to the model's provider, or the API you set in `MODEL_API_URL` (see [other providers](docs/MODELS.md#other-providers)). With [no model](docs/MODELS.md#no-model), nothing is sent anywhere:

- its instructions (the built-in prompt, plus `MODEL_SYSTEM_PROMPT`, `INTRO` and any MCP server's `instructions`);
- the chat's recent messages, with the bot's replies: at most `HISTORY_MAX` messages (the older half dropped when full), none older than a day. In a group this is the whole conversation, including messages not addressed to the bot, so it can answer "what do you think?" about something just said; set `GROUP_CONTEXT=false` to keep only the messages addressed to it;
- the new message, with the display name of whoever sent it (in groups) and of anyone it @mentions;
- the quoted text, when someone replies to one of the bot's messages, or mentions the bot in reply to someone else.
- files in the message, or in the message it replies to: a PDF or audio as itself (on OpenRouter, a PDF for a model that can't read files is read by OpenRouter's OCR service, `mistral-ocr`), and text, Word, Excel and PowerPoint files as their text, for that one request. The bot never keeps a file, and history notes only that it was attached;
- photos or a sticker in the message (a sticker also by its emoji and pack title), and the thumbnail of a quoted photo, when the model accepts images (on OpenRouter the bot checks its model list at startup; with another provider it sends them until the API rejects one).

With web search on (`MODEL_SEARCH`), the model decides whether to search and writes the query itself. The query can contain anything the model was sent (see above), such as a name or a phrase from a message, and goes to the search service OpenRouter uses. Web search runs only on OpenRouter.

When a message that addresses the bot has a link, or replies to one with a link, the bot fetches the page from its own machine, so the site sees that machine's address and the user agent `signalbot (+https://github.com/robdevops/signalbot)`, never who sent the link. The page's text then goes to the model's API with the message. The bot never fetches a link the model or a page suggests, never a private or local address, and never a link that looks private or one-time (a reset, confirmation, unsubscribe or webhook link, or one carrying a token, key or signature), so it can't spend one. To cap links at 20 an hour per chat it counts, in memory, when each chat last had links fetched, never which.

Charts are drawn on the bot's own machine from the numbers the model passes in; nothing extra leaves it.

When asked for a picture, the model writes a prompt for the image model, which can quote anything above, such as a name or what someone said. The prompt goes to OpenRouter's images endpoint, with the same `data_collection: deny` request, and on to the image model's provider (OpenAI's, for GPT Image 2). The picture comes back to the bot and is sent to the chat; the bot keeps only a note of the prompt in history, as above, and logs neither.

With MCP tools configured, the model may call them. A call sends the arguments the model wrote, which can quote anything above, to that tool's MCP server, and the tool's result goes to the model's API with the next request.

With an [addon](#addons) configured, the bot sends that program, on the same machine, what its permissions allow: see below. Whatever the addon then sends elsewhere is up to it.

Every request to OpenRouter tells it to use only model providers that do not collect data (`data_collection: deny`). That is a request the bot cannot check or enforce, so read OpenRouter's policy and the model provider's. Other APIs get no such request, since it is OpenRouter's own; read their data policy. A local server, such as Ollama on the same machine, keeps everything on it.

## What the bot keeps

- **With no model, no history at all.** The bot keeps no messages, not even group context, since nothing would read them.
- **History lives in memory only.** It is kept per chat, up to `HISTORY_MAX` messages and 500 chats, for at most 24 hours, or until a shorter disappearing-messages timer runs out. A restart clears it, and the bot never writes it to disk. In groups it holds messages not addressed to the bot too (their text only), unless `GROUP_CONTEXT=false`. Photos are not kept at all: the history notes that one was sent, and the bot fetches a photo from signal-cli only while answering a message that includes it.
- **Deletes and edits.** The bot honours deletes and edits as the Signal app does: from whoever wrote the message, even someone no longer allowed to DM the bot, and a group admin's delete of anyone's message. As in the app, a blocked person's deletes don't reach it, since signal-cli drops everything from them. A message deleted for everyone is dropped from the bot's history, along with any turn that quoted it, and its attachments from the recent ones below. If it is still waiting to be answered it is never sent to the model; if the model is answering it at that moment, the answer is sent but neither is kept. An edited message replaces the original in history the same way: the bot keeps only the message as it now reads. The bot's own messages stay until they expire, unless the account's owner deletes or edits one from a linked device, which drops it.
- **View-once photos and files** sent to the bot are shown to the model like any other, and remembered as below like any other attachment: the bot is a participant, and sees what it is sent.
- **No swap or core dumps.** The included systemd unit and Docker command forbid both, so history cannot reach disk that way.
- **Memory is freed, not wiped.** An expired message is dropped and Go's garbage collector reclaims its memory, but the bytes may linger until overwritten, and someone with root access to the host could read them from the running process.
- **What features save, on disk.** Features save state only through the core, one file each in the state directory (`/var/lib/private/signalbot`), so everything the bot keeps on disk is listed here.
- **Schedules are kept on disk.** Each `/schedule` saves, in `schedules.json` in the state directory (`/var/lib/private/signalbot`), the scheduling user's ID, the group's ID and its name when scheduled (if it's for a group), the time and repeat, and the text to post, until the user deletes it with `/schedule del`. The file also keeps, for each user who has scheduled, the last schedule number they were given, so numbers are never reused; it stays after their schedules are deleted. A one-off dropped because the bot was down is deleted, and its user is told in their DM, with the first 80 characters of its text. Besides today's spending, with an addon `mcp.json` (both below), on Telegram the newest 300 answers to MCP [direct commands](docs/FILES_LINKS_TOOLS_MCP.md#direct-commands) (`mcp-posts.json`: the chat's ID, the message's, the command and its value, so the buttons under them work after a restart), the chats subscribed to an MCP [feed](docs/FEEDS.md) (`mcp-feeds.json`: the chat's ID, the kinds it turned off and its sound setting, until `/alerts off`, and the IDs of the feed's items), and a Telegram DM's reply keyboard set by `/keyboard` (`keyboards.json`: its labels and commands, under a SHA-256 hash of the chat, which could be matched by trying every ID, until `/keyboard off`), nothing else the bot keeps is written to disk; an addon's own `addons/<name>` directory is the addon's (below).
- **Today's spending, on disk.** With a daily budget (`DAILY_BUDGET`, on by default), `budget.json` in the state directory holds today's date and, for each chat that used the model today, what it spent and whether it was told it reached the limit. Chats are stored under a SHA-256 hash of their ID, so the file holds no phone numbers, account IDs or group IDs as such. A hash of a phone number could be matched by trying every number, so treat the file as you would the schedules. Each day's file replaces the last. `/usage` shows a chat its own day, and anyone who can use it, in a group or a DM, the API key's totals and remaining credit.
- **Addon state, on disk.** With an MCP server configured as an addon, `mcp.json` in the state directory holds what is listed under [Addons](#addons). Without one, the file is never written.
- **Recent attachments, in memory only.** So that "@bot summarise this" in reply to someone's file can fetch the original, the bot remembers, for each group message and each DM from an allowed sender with attachments, the attachments' IDs, file names, types and sizes, never their content, under the chat, sender and time. Each is forgotten after a day, or sooner when 2,000 newer messages are remembered, and a restart clears them all. On Signal the bot also keeps a copy of each picture it sent (a chart, a picture it made), content and all, for at most a day and 32 MB in all, since signal-cli keeps none: a reply quoting it then shows the model the picture rather than Signal's small thumbnail. A delete of that message from the account's linked device drops it, and a restart clears them all.
- **Names.** The bot keeps a copy of signal-cli's contact list (names and IDs) in memory, so it can show and mention people by name. It reloads the list at startup, after reconnecting, when a group changes, and at least daily when in use, replacing what it had, so people who leave a group are forgotten. It also keeps the name of anyone who sends it an addressed message. It never keeps what anyone wrote.

## What the bot logs

Every level, debug included, is always written; there is no setting to turn any of it down.

- **A message addressed to the bot:** "received group chat" or "received dm" ("(edit)" after it for an edit), with no name, ID or text; after the answer, a line with the time taken, tokens used and cost (when the API reports it), again with no name or ID, or "reacting instead of replying" when the answer is a reaction.
- **Never the text of any message,** nor errors that quote one.
- **A message not addressed to the bot:** nothing, not even a note that it arrived.
- **Reactions and changes in a group's members:** nothing. They are passed to features that ask for them (who reacted, with what, to whose message; who joined or left), never logged, kept or shown to the model. With no such feature, they are dropped as they arrive.
- **A DM from someone not in `SIGNAL_DM_USER_IDS`:** their ID, every time, never a name or text. The same goes for a scheduled run in the DM of a user since removed from the list.
- **A tool call:** the tool's name and how long it took; never the arguments or the result. An addon's calls, made by the bot itself, are logged the same way, with the result's size and code, as are counts of what it had dropped; never what was said, nor the addon's own error text.
- **A link, file, photo or sticker in a message addressed to the bot:** one line each with what the bot did with it and, once it was fetched, its size (how much text was read, a PDF or audio attached, an image shrunk, or why it failed); one line when a link wasn't fetched (it looked private or one-time, it wasn't on port 80 or 443, the chat's 20 an hour were used, or it was past the second in a message) or when the model can't see the images or sticker; never the link, not even its host, the file's name or the type it was sent as (a file the bot can't read is logged with the type sniffed from its content), the page's title, a sticker's emoji or pack title, or any text.
- **Schedules:** how many were loaded at startup, a run skipped or dropped, and how long each run took; never the text or who made it. A run dropped because its user left the group is logged without content.
- **Names and IDs:** never anyone's name, and never anyone's ID except as above for a DM from someone not in `SIGNAL_DM_USER_IDS`. The bot logs its own account ID at startup.
- **Debug lines:** the number of contacts loaded, that a repeated message was dropped, and a local MCP server's own error output, which the bot doesn't control, so each line has every name and ID the bot knows replaced with "redacted" ("Alice's" becomes "redacted's"), along with any UUID or phone number, before it is written. An addon's error output is never written at all, only counted ("addon geo wrote 3 lines (210 bytes) to stderr", at most once a minute, and once more when it exits), since it may repeat what it was sent.

Logs go to the journal or Docker's log driver and are kept as long as your settings keep them.

## What the bot can see

In a group, signal-cli passes every message to the bot, which checks each one for whether it is addressed to it: it mentions the bot, replies to it, or starts with one of its commands, such as `/intro`. Messages not addressed to it are never logged and never answered. Their text, with the sender's display name, is kept in that group's history as context, the same way and for the same time as everything else there, unless `GROUP_CONTEXT=false`, in which case they are dropped on the spot. In DMs, the bot answers only the IDs listed in `SIGNAL_DM_USER_IDS`. A message an addon's watch takes (see below) is neither answered nor kept, even as context.

## Telegram

On Telegram (`TELEGRAM_BOT_TOKEN` set) the above holds, with these differences:

- **Not end-to-end encrypted.** Telegram's servers see every message in a bot's chats, and every reply. Telegram's own policy applies to them.
- **Deletes don't reach the bot.** The Bot API never tells a bot a message was deleted, so a deleted message stays in the bot's history until it expires (at most a day, or the chat's auto-delete timer). An edit does reach it, and replaces the original as on Signal.
- **What the bot sees.** With privacy mode on (the default), a group sends the bot only messages addressed to it, so it keeps no group context. With privacy mode off, or as an admin, it gets every message, kept as context as above.
- **Names and groups.** The Bot API has no contact list or member list. The bot keeps, in memory, the names of people it has seen post and the groups it has seen, and asks Telegram whether someone is still in a group when it needs to; a restart clears them.
- **Photos and files** are fetched from Telegram's servers by file ID, only while answering the message that has them.
- **Who it answers in groups.** Anyone can add a Telegram bot to a group, so the bot answers in a group only while someone in `TELEGRAM_DM_USER_IDS` is a member, and leaves one where no one is. Until it has checked, a stranger's group can briefly reach it: such messages are never answered, and chatter is dropped once the check is done.
- **Topics.** In a group with topics, each topic keeps its own history.
- **Locations.** A shared location or venue reaches the bot as a map link with its coordinates, as on Signal, so it is kept, and sent to the model, like any text. A live location, which follows its sender, is left out.
- **The token** is in every request's URL; the bot takes it out of every error before logging it.

## Addons

An addon is a program the bot runs on its own machine, as an [MCP server](docs/FILES_LINKS_TOOLS_MCP.md#addons), that answers its own commands, reads the messages it asks for and posts on its own, such as the [GeoGuess](https://github.com/robdevops/GeoGuess) game. With none configured, none of this applies.

- **It is trusted code.** It runs as the bot's user, in the same sandbox, and could read what the bot's user can, the state directory included. What follows limits what the bot hands it; it guards against mistakes and leaks, not against a hostile program. Install only addons you would let run as the bot.
- **What it never gets:** phone numbers, account IDs or group IDs. Chats and people are pseudonyms, made from a secret in `mcp.json` and different for each addon; by default the same person has a different one in each chat, so an addon can't follow them between groups. It gets none of the bot's settings or keys either: only `PATH`, `HOME`, the time zone and proxy settings (`TZ`, `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`, `SSL_CERT_FILE`, `SSL_CERT_DIR`) if set, and the variables named for it (such as `GEO_…`).
- **Where it works:** only in the groups its `chats` lists (or all, if set to `all`), and in DMs only with `dms`.
- **What each permission sends:**
  - a command (`/geo …`) sends the text after it, and who sent it, as a pseudonym, and when the bot got it, never the sender's own clock;
  - `watch` sends the messages its watch matches (GeoGuess: map links and coordinates during a round), with the same; nothing else said in the chat. A pattern that would match ordinary chat is refused, so there is no feed of everything. A reply watch sends replies to the addon's own messages that ask the bot, such as answers to a quiz, whatever they say;
  - `names` adds each sender's display name. Without it, the addon writes a pseudonym where a name goes, and the bot puts in the name it last saw that person use in that chat, kept in memory for a day;
  - `global_ids` gives a person the same pseudonym in every chat, so the addon can link them across groups;
  - `post` and `react` send nothing: they let it post and react, only in a chat where it was used in the last day;
  - `model` lets it ask the bot's model, with the bot's key: the addon's prompts, and whatever it put in them from the chat, go to the model provider, as chat with the bot does, and what they cost counts toward that chat's `DAILY_BUDGET`; only for a chat that used the addon in the last day. The bot logs only each request's outcome, time, size and cost, never the prompt or the answer.
- **What the addon keeps** is its own, outside what this page lists for the bot, in the directory its `…_STATE_DIR` names (GeoGuess: `GEO_STATE_DIR`): by default `addons/<server name>` in the state directory (`/var/lib/private/signalbot/addons/geo`), which the addon creates and writes, never the bot. Read its own documentation.
- **Deletes and edits are not passed on.** A message an addon took stays with the addon if its sender later deletes it for everyone. An edit of one is passed on, as a new version.
- **Taken messages are kept nowhere by the bot:** not in history, not as context (so `GROUP_CONTEXT=false` makes no difference to them), not in the log. The addon still gets them.
- **What it posts** is kept in history as the bot's own words, labelled with the addon's name (`[GeoGuess] …`) and cut to 500 characters, so later model requests can see it but it never poses as a person.
- **`mcp.json`,** in the state directory, written only when some MCP server is an addon:

  | Item | Kept for |
  |---|---|
  | The pseudonym secret | Until `mcp.json` is deleted, which gives everyone new pseudonyms |
  | Chats an addon may post in: the chat's pseudonym and its real ID | 24 hours after the addon was last used there |
  | Live watches: the chat's pseudonym, the pattern, and the times of the bot's messages with the command's pictures, and for a reply watch of the addon's messages | Until the watch ends, at most `watch_max` (24 hours at most) |
  | Messages a watch took: the chat's ID, the sender's pseudonym and the message's time, never its text | 48 hours, so a later edit is taken too |
  | How far the bot has read each addon's outbox, and the last 1,000 outbox entries' IDs | Replaced on each save; dropped once the server is no longer an addon |
  | An addon's last 256 posts, for a later post to reply to or react to: the post's ID, the chat's pseudonym and the time (on Telegram, the ID) of the bot's message, never its text | 24 hours |

  Refs to messages (for reactions, a day), display names (a day) and posts waiting to go out are kept in memory only.

## What this does not cover

- **The provider.** Once a message is sent, the model provider's retention, logging and sub-processors apply.
- **End-to-end encryption.** On Signal, its encryption protects messages between people. The bot is a participant: signal-cli decrypts what it receives, and what it answers goes to the provider. People in a chat should know the bot is there.
- **signal-cli's own data.** The account keys and whatever else signal-cli stores sit in its data directory, which the bot does not read. Keep it private (see the [README](README.md#prerequisites)).
- **Anyone on the host, with the default TCP connection.** signal-cli listens on `127.0.0.1:7583`, and any local user or process can connect to it and read or send messages as the Signal account. On a shared host, use signal-cli's [Unix socket](docs/SOCKET.md), which only a Unix group can use; then anyone in that `signal-cli` group (not a Signal group) can drive the account, so keep it to just the bot.
- **The host.** Root users can read process memory, and log backups keep what the logs contain.

---

[Back to README.md](README.md)
