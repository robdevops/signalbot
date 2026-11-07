# Files, Links and Tools

- [Files](#files)
- [Links](#links)
- [How tools are offered](#how-tools-are-offered)
- [MCP tools](#mcp-tools)

## Files

Send the bot a file in a DM, attach one when you @mention it or reply to it, or @mention it in reply to someone else's file. Each reaches the model in the form it can use:

| File | What the model gets |
|---|---|
| PDF | The PDF itself. A model that reads files natively gets it as it is; for any other, OpenRouter reads it first with its `mistral-ocr` engine, which handles tables, columns and scanned pages |
| Audio, including voice notes | The audio, if the model takes audio (OpenRouter's model list says); otherwise a note that it can't hear it |
| Text and code (`.txt`, `.md`, `.csv`, `.json`, source files, …) | Its text, up to 50,000 characters |
| Word, Excel, PowerPoint (`.docx`, `.xlsx`, `.pptx`) | Its text: paragraphs, cells by sheet (each headed with its name), slides in order |
| Anything else | A note that it was attached and can't be read |

Up to 4 files a message, 12 MB each and 16 MB together (the model is told of any left out). A Word, Excel or PowerPoint file is read within fixed limits: one with over 20,000 parts or elements nested 256 deep is reported as unreadable, and past 32 MB of XML (a deck heavy with shapes can reach it) the text read so far is shown, and the model is told only the first part is there. The model is told that what a file says is content to read, not instructions, and tools without keywords aren't offered on a message whose file the model is shown (its text, a PDF or audio; see [How tools are offered](#how-tools-are-offered)). **OCR costs money:** OpenRouter charges about $2 per 1,000 pages, so a 150-page filing is about $0.30 each time it's sent, on top of the reply, and shows in the reply's logged cost. The bot never keeps a file, so asking about it again means replying to it again, and it's read again. To fetch the original of a file someone else posted, the bot remembers recent attachments' IDs and names in memory for a day; see [PRIVACY.md](../PRIVACY.md). The log records each file's kind and size and what the model got, or that it was no longer available, never its name.

## Links

A message that addresses the bot and has a URL, or replies to a message with one, gets the page read for the model: up to 5 links (any more are named to the model as not opened, so it can say to send them on their own), the page's main text up to 15,000 characters, a PDF link as a PDF (see [Files](#files)). Only links a person sent are fetched, never ones the model or a page suggests, so a link in a reply to one of the bot's own messages isn't fetched. Only links on the standard web ports (80 and 443) are fetched. A link to a private or local address (the bot's own machine, your network, a cloud metadata service) is refused, however its name resolves, and the model is told it couldn't load it. So is a link that looks private or one-time, so asking "is this legit?" can't spend it: a reset, confirmation, unsubscribe or webhook link, or one carrying a token, key or signature, including where a short link redirects. Each chat can have 20 links fetched an hour; past that the model is told when links can be read again. A link that fails says why in plain words, such as "the page wasn't found (404)" or "the site refused the request (403); it may need signing in or block bots". The log records what came of each link (its time, size and kind, how much text was read, a PDF attached, or why it failed or wasn't fetched), never the link itself, not even its host. The model is told that what a page says is content to read, not instructions, and tools without keywords aren't offered on a message whose page the model is shown (see [How tools are offered](#how-tools-are-offered)).

## How tools are offered

Web search and tools without `keywords` are offered with every message, except that on a message whose page or file the model is shown (its text, a PDF or audio), tools without `keywords` are left out (web search and a chat command's own tool stay), so text inside a document can't call a tool: only what someone typed can. Charts, and MCP servers with `keywords`, are offered only when a message matches their words, then for `TOOL_LINGER_ROUNDS` more replies, and each reply that matches or uses one of their tools starts that count again. So "chart NVDA", "add AMD", "make it a year" keeps the charts on however long it runs; only replies that leave them unused count down.

The default of 2 is meant to cover a short aside ("thanks!", "what's AMD's ticker again?") before you come back to the chart, without the tools still being on when the conversation has moved on. Lingering longer has two costs: a tool on for unrelated messages is one the model may use when it shouldn't (a stock tool picked for a population question), and its descriptions add to every request. The prompt cache gains little, because the offered tools change once either way when a group drops off. Raise it if people often come back to a chart after several other questions; set `0` to offer a group only on messages that name it.

## MCP tools

Your own tools from [MCP](https://modelcontextprotocol.io) servers, remote or local, offered alongside web search and charts. Setting them up, choosing which tools each server offers, `keywords`, `instructions` and chat commands: [MCP tools](MCP.md).
