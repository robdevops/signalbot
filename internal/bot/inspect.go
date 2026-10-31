package bot

import (
	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/history"
)

// These let tests outside the package see the bot's state, read-only.

// Busy is the number of concurrency slots in use.
func (b *Bot) Busy() int { return len(b.sem) }

// History is a chat's remembered entries.
func (b *Bot) History(c chat.ChatID) []history.Entry { return b.hist.Get(c) }

// SystemPrompt is the system prompt the bot would send for message m, and
// the names of the tools it would offer.
func (b *Bot) SystemPrompt(m chat.Message) (string, []string) {
	p := b.prepare(newRequest(m), b.introName(m.Chat))
	return p.system, p.turn.Offered
}

// UserTurn is the text the bot makes of message m before features add to
// it, and the line it logs for m.
func UserTurn(m chat.Message) (prompt, logLine string) {
	r := newRequest(m)
	return r.Prompt, r.logLine()
}
