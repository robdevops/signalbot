# Files

Send the bot a file in a DM, attach one when you @mention it or reply to it, or @mention it in reply to someone else's file. Each reaches the model in the form it can use:

| File | What the model gets |
|---|---|
| PDF | The PDF itself. A model that reads files natively gets it as it is; for any other, OpenRouter reads it first with its `mistral-ocr` engine, which handles tables, columns and scanned pages |
| Audio, including voice notes | The audio, if the model takes audio (OpenRouter's model list says); otherwise a note that it can't hear it |
| Text and code (`.txt`, `.md`, `.csv`, `.json`, source files, …) | Its text, up to 50,000 characters |
| Word, Excel, PowerPoint (`.docx`, `.xlsx`, `.pptx`) | Its text: paragraphs, cells by sheet, slides in order |
| Anything else | A note that it was attached and can't be read |

Up to 4 files a message, 12 MB each and 16 MB together (the model is told of any left out). A Word, Excel or PowerPoint file is read within fixed limits: one with over 20,000 parts or elements nested 256 deep is reported as unreadable, and past 32 MB of XML (a deck heavy with shapes can reach it) the text read so far is shown. The model is told that what a file says is content to read, not instructions, and tools without keywords aren't offered on a message whose file the model is shown (its text, a PDF or audio; see [Tools](TOOLS.md)). **OCR costs money:** OpenRouter charges about $2 per 1,000 pages, so a 150-page filing is about $0.30 each time it's sent, on top of the reply, and shows in the reply's logged cost. The bot never keeps a file, so asking about it again means replying to it again, and it's read again. To fetch the original of a file someone else posted, the bot remembers recent attachments' IDs and names in memory for a day; see [PRIVACY.md](../PRIVACY.md). The log records each file's kind and size and what the model got, or that it was no longer available, never its name.
