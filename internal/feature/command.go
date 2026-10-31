package feature

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode"
)

var (
	commandsMu sync.Mutex
	commands   = map[Host]map[string]bool{} // the names taken on each host
	validName  = regexp.MustCompile(`^[a-z0-9_-]+$`)
)

// Command registers /name on h: an addressed message whose text is "/name"
// or "/name args", the name in any case, gets fn's Action. A name is
// lowercase letters, digits, _ or -, and is refused if another feature has
// taken it on h.
func Command(h Host, name string, fn func(ctx context.Context, m Message, args string) Action) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("command /%s: a name is lowercase letters, digits, _ or -", name)
	}
	commandsMu.Lock()
	defer commandsMu.Unlock()
	if commands[h] == nil {
		commands[h] = map[string]bool{}
	}
	if commands[h][name] {
		return fmt.Errorf("command /%s is already taken", name)
	}
	commands[h][name] = true
	h.OnMessage(func(ctx context.Context, m Message) Action {
		if n, args, ok := ParseCommand(m.Text); ok && m.Addressed && n == name {
			return fn(ctx, m, args)
		}
		return Action{}
	})
	return nil
}

// ParseCommand splits "/name args" into the lowercased name and the args.
func ParseCommand(text string) (name, args string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	name, args = text[1:], ""
	if i := strings.IndexFunc(name, unicode.IsSpace); i >= 0 {
		name, args = name[:i], strings.TrimSpace(name[i:])
	}
	return strings.ToLower(name), args, name != ""
}
