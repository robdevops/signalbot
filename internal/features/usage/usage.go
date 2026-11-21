// Package usage answers "/usage", and its aliases "/credits" and
// "/tokens", without the model: what the model has cost in this chat
// today, against the daily budget, and the API key's own totals.
package usage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
)

// Register adds /usage, /credits and /tokens to h.
func Register(h feature.Host) error {
	answer := func(ctx context.Context, m feature.Message, args string) feature.Action {
		if args != "" {
			return feature.Action{} // only the command itself; anything more carries on
		}
		if h.Info().Model == "" {
			return feature.Action{Answer: "There's no model, so nothing is spent."}
		}
		return feature.Action{Answer: report(h.Usage(ctx, m.Chat, true))}
	}
	names := []string{"usage", "credits", "tokens"}
	for _, n := range names {
		if err := h.Command(n, answer); err != nil {
			return err
		}
	}
	h.ListCommands("usage and credits", names...)
	return nil
}

// report is the answer: this chat's day, then the key's totals.
func report(u feature.Usage) string {
	var lines []string
	if u.Tracked {
		pct := 0.0
		if u.Limit > 0 {
			pct = 100 * u.Spent / u.Limit
		}
		lines = append(lines, fmt.Sprintf("**This chat today:** %s of %s (%.0f%%) · %s tokens in, %s out · resets in %s",
			dollars(u.Spent), dollars(u.Limit), pct, count(u.In), count(u.Out), until(u.Resets)))
	} else {
		lines = append(lines, "**This chat today:** not counted, as there's no daily budget (DAILY_BUDGET)")
	}
	switch k := u.Key; {
	case k == nil:
		lines = append(lines, "**Account:** the API doesn't say what the key has spent")
	default:
		line := fmt.Sprintf("**Account (API key):** %s today · %s this week · %s this month", dollars(k.Today), dollars(k.Week), dollars(k.Month))
		if k.Limit != nil && k.Remaining != nil {
			line += fmt.Sprintf(" · %s of %s left", dollars(*k.Remaining), dollars(*k.Limit))
			if k.Reset != "" {
				line += " (resets " + k.Reset + ")"
			}
		} else {
			line += " · no credit limit on the key"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// dollars is d in US dollars: to the cent, or to a tenth of a cent below
// ten cents, where most chats' days are.
func dollars(d float64) string {
	if d > 0 && d < 0.1 {
		return fmt.Sprintf("$%.3f", d)
	}
	return fmt.Sprintf("$%.2f", d)
}

// count is n with thousands separators.
func count(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// until is d in whole hours, or minutes under an hour.
func until(d time.Duration) string {
	if d >= time.Hour {
		h := int((d + 30*time.Minute) / time.Hour)
		if h == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", h)
	}
	m := max(int(d/time.Minute), 1)
	if m == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", m)
}
