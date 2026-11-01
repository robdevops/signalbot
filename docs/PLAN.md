# Plan: what's next

Items 1–4 and 7 are done (Markdown prompt, Signal text styles, bold tool labels, the A− items, optimistic capabilities). Item 5 was dropped. Items 6, 8 and 9 remain.

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

6. **Signal reduction pass,** using the lean criteria, with a target of under 1,150 lines. Finish with a size report per package against `fbba3aa`.

Behaviour changes, approved: the prompt asks for Markdown, and replies can show bold, italic, code and spoilers.

8. **Refuse to start when the state directory can't be written.** At startup, `/schedule` checks that its directory (`$STATE_DIRECTORY`, or the working directory) can be listed and written. If not, the bot exits with a clear message saying to add `StateDirectory=signalbot` to the systemd unit, the way the Docker `/data` check already does. Approved.
9. **Tell the model about the scheduling commands.** The prompt's "If asked what tools you have" list names `/schedule`, `/timer`, `/reminder` and `/job`, so a near-miss like `/time …` gets pointed to the right command instead of "I can't set reminders". This needs a command-only entry the core keeps although it has no model tools. No `/time` alias. Approved.
