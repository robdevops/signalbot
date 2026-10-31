package signal

import (
	"fmt"
	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/kit/bound"
	"github.com/robdevops/signalbot/internal/kit/txt"
	"maps"
	"strings"
	"unicode/utf16"
)

// buildMentions returns signal-cli mention specs ("start:length:uuid") for the
// known @names in text. Offsets are UTF-16 code units. An ambiguous name
// (empty uuid) stays plain text.
func buildMentions(text string, known map[string]string) []string {
	rs := []rune(text)
	var out []string
	u, pos := 0, 0 // UTF-16 offset of rs[pos]
	for _, s := range txt.AtNames(rs, known) {
		u += utf16Len(rs[pos:s.Start])
		length := utf16Len(rs[s.Start:s.End])
		if s.ID != "" {
			out = append(out, fmt.Sprintf("%d:%d:%s", u, length, s.ID))
		}
		u += length
		pos = s.End
	}
	return out
}

func utf16Len(rs []rune) (n int) {
	for _, r := range rs {
		n += utf16.RuneLen(r)
	}
	return n
}

// remember records the names the model is shown in a group message (the
// sender's, and those of people mentioned), so "@Name" in a reply can become
// a real mention.
func (a *Adapter) remember(c chat.ChatID, env *Envelope, self string) {
	seen := map[string]string{}
	add := func(n, uuid string) {
		if n = txt.Safe(n); n != "" && uuid != "" && uuid != self {
			bindName(seen, n, uuid)
		}
	}
	add(env.SourceName, env.SourceUUID) // the name the model sees in the prompt
	for _, m := range env.DataMessage.Mentions {
		if m.Number != self {
			add(a.names.Resolve(m), m.UUID)
		}
	}
	if len(seen) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	bound.MakeRoom(a.known, c, maxChats)
	k := a.known[c]
	if k == nil {
		k = map[string]string{}
		a.known[c] = k
	}
	for n, uuid := range seen {
		if _, ok := k[n]; ok || len(k) < 256 {
			bindName(k, n, uuid)
		}
	}
}

// bindName records that name n refers to uuid. Display names are self-chosen,
// so anyone can claim a member's name: a name claimed by two different people
// is ambiguous, marked "", and never linked.
func bindName(m map[string]string, n, uuid string) {
	n = strings.ToLower(n) // mentions match case-insensitively, so "alice" claims "Alice"
	if old, ok := m[n]; !ok {
		m[n] = uuid
	} else if old != uuid {
		m[n] = ""
	}
}

func (a *Adapter) knownNames(c chat.ChatID) map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.known[c])
}
