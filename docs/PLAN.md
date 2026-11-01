# Plan: what's next

Items 1–4 and 6–9 are done (Markdown prompt, Signal text styles, bold tool labels, the A− items, the Signal reduction pass, optimistic capabilities, the state-directory check, the model knowing the scheduling commands). Item 5 was dropped.

Do each remaining item, run gofmt, `go vet` and `go test -race ./...`, then commit and push.

1. **The prompt asks for standard Markdown, from the core.** "Reply in Markdown" replaces "plain text: Signal does not render markdown"; the brevity rules stay. The network supplies only its own lines ("a Signal chat", the 2,000-character limit). This removes the last Signal wording from the core. Re-pin both golden prompts.
2. **Signal's Markdown converter, applied on send:**
   - `**bold**`, `*italic*` and `_italic_`, `~~strike~~`, `` `code` `` and `||spoiler||` become signal-cli `textStyle` ranges (`"start:length:STYLE"`, in UTF-16 units like mentions);
   - code blocks become monospace without the fences, `# headings` a bold line, and `[text](url)` becomes `text (url)`;
   - lists and tables are left as written; unmatched markers, and markers inside words or code, are left alone;
   - conversion runs before splitting, and styles are carried into each part.

   First check that the installed signal-cli supports `--text-style`.
3. **Bold tool-category labels in the intro.** Each tool group writes its entry in Markdown: `**charts** (line, pie)`, `**stocks** (get_quote, …)`, `**web search**`. This shows in the intro, the closing tools list and `/intro`. Re-pin the goldens and the `/intro` scenarios.
4. **A− items** (done):
   - `feature.Received` moves out of the feature API into `internal/bot`, next to `Network`; signal imports bot, network to core;
   - `Network.Delivered` is simplified to the message plus ok: no callback, mention detection becomes a field, and the names to remember come back with the message;
   - keep the mention names on `Send` and the caption rule as they are, and document why.
7. **Optimistic capabilities on other providers (not OpenRouter, which keeps its model list)** (done):
   - **Images:** assume images work. If a request carrying images is rejected (a 4xx), the model client returns a distinct "images rejected" error. The bot turns images off for the rest of the run, logs one warning (no content), and redoes the same turn without them. Vision then says "[N image attached, which you can't see with this model]", so the user gets an answer instead of "Sorry…".
   - **Tools:** the same for tools. If a request carrying tools is rejected, turn tool calling off for the rest of the run, warn once, and redo the turn without them.
   - **Tests:** a rejected image request falls back and is answered, with exactly one failed request; the same for tools.
   - About 35 lines. Documented in README "Other providers" and TECHNICAL.md.
   - After review: only a 400 or 422 counts as a rejection (not 404, 408, 413 and the like); images rejected alongside tools come back on when the request without them is rejected for its tools; once tools are off, a chat command answers that it needs tools.

6. **Signal reduction pass,** using the lean criteria, with a target of under 1,150 lines. Finish with a size report per package against `fbba3aa`. (done)
   - The target predates the Markdown converter, so it is read as excluding `markdown.go`. `internal/signal` went from 1,557 lines to 1,441: 1,326 to 1,234 without the converter, and the converter from 231 to 207. That misses 1,150 by about 85 lines. What is left is the behaviour itself and the comments that explain why, and cutting further would mean dropping one or the other.
   - Cuts: `stripAt`, `data`, `group.has`, `utf16Len`, `stale`, `set` and `link` (now one `learn`), and the `unnamed` constant are gone. `groups` filters by member and keeps the members, so `Delivered` and `Groups` share it. One `is` helper replaces each hand-written "is this ID one of these" check. A single table of UTF-16 offsets serves both mentions and text styles. In the converter, a code block's lines are joined, `span` adds the plain rune itself, and `opens` uses `repeats`. Comments that restated the code were trimmed.
   - Behaviour is unchanged: every scenario and golden passes as it was. Signal's own tests changed only where a helper they called went away: `TestStrip` calls `strip`, groups are checked through `Groups`, and names through `learn`.

   Sizes against `fbba3aa` (`go run ./internal/arch/sizes`):

   | package | lines | change |
   |---|---:|---:|
   | . | 91 |  |
   | internal/bot | 737 | +109 |
   | internal/chats | 327 | -3 |
   | internal/config | 92 | +1 |
   | internal/feature | 221 | -7 |
   | internal/features/charts | 1136 |  |
   | internal/features/example | 0 | -43 |
   | internal/features/groupctx | 44 |  |
   | internal/features/intro | 19 | +19 |
   | internal/features/mcp | 714 |  |
   | internal/features/schedule | 765 | +17 |
   | internal/features/vision | 93 |  |
   | internal/features/websearch | 36 | +4 |
   | internal/kit/httpx | 18 |  |
   | internal/kit/img | 23 |  |
   | internal/kit/logx | 142 |  |
   | internal/kit/txt | 39 |  |
   | internal/model | 422 | +39 |
   | internal/signal | 1441 | +150 |
   | internal/timers | 131 |  |
   | **total** | **6491** | +286 |


Behaviour changes, approved: the prompt asks for Markdown, and replies can show bold, italic, code and spoilers.

8. **Refuse to start when the state directory can't be written** (done). At startup, `/schedule` checks that its directory (`$STATE_DIRECTORY`, or the working directory) can be listed and written. If not, the bot exits with a clear message saying to add `StateDirectory=signalbot` to the systemd unit, the way the Docker `/data` check already does. Approved.
9. **Tell the model about the scheduling commands** (done). The prompt's "If asked what tools you have" list names `/schedule`, `/timer`, `/reminder` and `/job`, so a near-miss like `/time …` gets pointed to the right command instead of "I can't set reminders". This needs a command-only entry the core keeps although it has no model tools. No `/time` alias. Approved. A `ToolGroup` with only `Names` is kept for that list alone, not `{tools}`.
