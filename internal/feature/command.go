package feature

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode"
)

var validName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// Commands is the command names taken on one host, for its Command: Take
// checks a name and refuses one already taken.
type Commands struct {
	mu    sync.Mutex
	taken map[string]bool
}

// Take claims name, or says why it can't be: not 1 to 32 lowercase
// letters, digits, _ or -, or already taken.
func (c *Commands) Take(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("command /%s: a name is 1 to 32 lowercase letters, digits, _ or -", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.taken[name] {
		return fmt.Errorf("command /%s is already taken", name)
	}
	if c.taken == nil {
		c.taken = map[string]bool{}
	}
	c.taken[name] = true
	return nil
}

// CommandHook is the OnMessage hook for command name: an addressed message
// that is "/name" or "/name args", the name in any case, gets fn's Action.
func CommandHook(name string, fn func(ctx context.Context, m Message, args string) Action) func(context.Context, Message) Action {
	return func(ctx context.Context, m Message) Action {
		if n, args, ok := ParseCommand(m.Text); ok && m.Addressed && n == name {
			return fn(ctx, m, args)
		}
		return Action{}
	}
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
