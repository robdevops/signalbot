package signal

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	maxNamesAge = 24 * time.Hour // a mention reloads an older list, for renames no group event reports
	maxNames    = 5000           // names kept, as IDs come from the network; a dropped one is learned again
	defaultName = "signalbot"    // the bot's name without a profile name
)

// Name ranks: a name is never replaced by one of lower rank.
const (
	rankUsername = 1
	rankSeen     = 2 // a sender's name on a live message
	rankGiven    = 3 // the person's own profile name
	rankContact  = 4 // the account owner's name for the contact
	rankNick     = 5 // the nickname the account owner gave them, which Signal shows over every other name
)

type rankedName struct {
	name string
	rank int
}

// names maps Signal IDs (ACI or number) to names, learned from senders and
// from signal-cli's contact list.
type names struct {
	mu          sync.Mutex
	byID        map[string]rankedName
	numbers     map[string]string // ACI to phone number, where both are known
	refreshedAt time.Time         // when a load was last tried
	loadedAt    time.Time         // when one last worked
	minRefresh  time.Duration
	loading     sync.Mutex  // held while the list loads, so a second caller waits for it
	pending     atomic.Bool // a reload is already scheduled
}

// contact is the part of a listContacts entry the bot uses.
type contact struct {
	Number    string `json:"number"`
	UUID      string `json:"uuid"`
	Username  string `json:"username"`
	NickName  string `json:"nickGivenName"`
	GivenName string `json:"givenName"`
	Profile   struct {
		GivenName string `json:"givenName"`
	} `json:"profile"`
}

// best is the contact's name: the account owner's nickname for them, then
// their name for them, then the contact's own profile name, then their
// username.
func (k contact) best() rankedName {
	for _, r := range []rankedName{{k.NickName, rankNick}, {k.GivenName, rankContact}, {k.Profile.GivenName, rankGiven}, {k.Username, rankUsername}} {
		if r.name = txt.SafeName(r.name); r.name != "" {
			return r
		}
	}
	return rankedName{}
}

// learn records r, unless it is blank, as the name of the person with this
// ACI and number, either of which may be "", and that both are theirs. The
// caller holds n.mu.
func (n *names) learn(r rankedName, aci, number string) {
	for _, id := range []string{aci, number} {
		if id != "" && r.name != "" && r.rank >= n.byID[id].rank {
			makeRoom(n.byID, id)
			n.byID[id] = r
		}
	}
	if aci != "" && number != "" {
		makeRoom(n.numbers, aci)
		n.numbers[aci] = number
	}
}

// makeRoom drops any one entry of m if it is full and lacks key.
func makeRoom[V any](m map[string]V, key string) {
	if _, ok := m[key]; !ok && len(m) >= maxNames {
		for k := range m {
			delete(m, k)
			break
		}
	}
}

func (n *names) lookup(ids ...string) rankedName {
	n.mu.Lock()
	defer n.mu.Unlock()
	var best rankedName
	for _, id := range ids {
		if r := n.byID[id]; id != "" && r.rank > best.rank {
			best = r
		}
	}
	return best
}

// replace swaps every name for signal-cli's current list, so people who
// left their groups are forgotten.
func (n *names) replace(cs []contact) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.byID, n.numbers = map[string]rankedName{}, map[string]string{}
	for _, k := range cs {
		n.learn(k.best(), k.UUID, k.Number)
	}
	n.loadedAt = time.Now()
}

// name is the best name known for a person by any of their IDs, or "".
func (c *Client) name(ids ...string) string { return c.names.lookup(ids...).name }

// Redact replaces each name the bot knows for anyone but itself with
// "redacted", as a whole word and ignoring capitals, longest first, so
// "Alice's" becomes "redacted's"; every UUID and phone number goes too. It
// is for text the bot logs but doesn't control, scrubbed as it passes.
func (c *Client) Redact(s string) string {
	self := c.self()
	c.names.mu.Lock()
	var known []string
	for id, n := range c.names.byID {
		if id != self && id != c.names.numbers[self] && utf8.RuneCountInString(n.name) >= 2 {
			known = append(known, n.name)
		}
	}
	c.names.mu.Unlock()
	slices.SortFunc(known, func(a, b string) int { return len(b) - len(a) })
	for _, name := range known {
		s = redactWord(s, name)
	}
	return idPattern.ReplaceAllString(s, "redacted")
}

// idPattern is a UUID or a phone number.
var idPattern = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b|\+[0-9]{7,15}\b`)

// redactWord replaces each whole-word w in s, ignoring capitals.
func redactWord(s, w string) string {
	if !strings.Contains(strings.ToLower(s), strings.ToLower(w)) {
		return s
	}
	var b strings.Builder
	last := 0
	for i := 0; i+len(w) <= len(s); {
		_, size := utf8.DecodeRuneInString(s[i:])
		before, _ := utf8.DecodeLastRuneInString(s[:i])
		after, _ := utf8.DecodeRuneInString(s[i+len(w):])
		if strings.EqualFold(s[i:i+len(w)], w) && !wordRune(before) && !wordRune(after) { // at an end, RuneError
			b.WriteString(s[last:i])
			b.WriteString("redacted")
			i += len(w)
			last = i
			continue
		}
		i += size
	}
	return b.String() + s[last:]
}

func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

// number is the phone number of the person with ACI aci, from the contact
// list or a message they sent, or "".
func (c *Client) number(aci string) string {
	c.names.mu.Lock()
	defer c.names.mu.Unlock()
	return c.names.numbers[aci]
}

// BotName is the bot's own profile name, or "signalbot", which loadNames loads.
func (c *Client) BotName() string { return cmp.Or(c.name(c.self()), defaultName) }

// mentionName is the name in a mention, made safe, unless it is the ID.
func mentionName(m mention) string {
	if m.Name != m.UUID && m.Name != m.Number {
		return txt.SafeName(m.Name)
	}
	return ""
}

// resolve is the best name for a mention, or "": a contact, profile or
// sender name, then the name in the mention, then a username.
func (c *Client) resolve(m mention) string {
	r := c.names.lookup(m.UUID, m.Number)
	if mn := mentionName(m); r.rank < rankSeen && mn != "" {
		return mn
	}
	return r.name
}

// loadNames learns the sender's name from env, and loads the contact list
// if the bot's own name is unknown, or for a mention of someone else when
// the list was never loaded, is over a day old, or has no real name for
// them. It may call signal-cli, so it runs in the message's slot.
func (c *Client) loadNames(ctx context.Context, env *envelope) {
	self, n := c.self(), &c.names
	n.mu.Lock()
	n.learn(rankedName{txt.SafeName(env.SourceName), rankSeen}, env.SourceUUID, env.SourceNumber)
	stale := n.refreshedAt.IsZero() || !n.loadedAt.IsZero() && time.Since(n.loadedAt) > maxNamesAge
	n.mu.Unlock()
	need := c.name(self) == ""
	if d := env.DataMessage; d != nil {
		for _, m := range d.Mentions {
			need = need || m.UUID != self && m.Number != self &&
				(stale || n.lookup(m.UUID, m.Number).rank < rankSeen && mentionName(m) == "")
		}
	}
	if need && c.refresh(ctx) && c.name(self) == "" {
		logx.Warnf("own profile name not found; calling itself %s (INTRO can name it)", defaultName)
	}
}

// refreshSoon loads the contact list as soon as the rate limit allows. A
// burst of calls folds into one load and one timer.
func (c *Client) refreshSoon() {
	n := &c.names
	if c.closed.Load() || !n.pending.CompareAndSwap(false, true) {
		return
	}
	n.mu.Lock()
	wait := max(0, n.minRefresh-time.Since(n.refreshedAt))
	n.mu.Unlock()
	time.AfterFunc(wait, func() {
		n.pending.Store(false)
		if !c.closed.Load() {
			c.refresh(context.Background())
		}
	})
}

// refresh loads the contact list, at most once per minRefresh, and reports
// whether it tried.
func (c *Client) refresh(ctx context.Context) bool {
	n := &c.names
	defer c.test.Begin()()
	// A caller that arrives mid-load waits, then finds the list fresh.
	n.loading.Lock()
	defer n.loading.Unlock()
	n.mu.Lock()
	if time.Since(n.refreshedAt) < n.minRefresh {
		n.mu.Unlock()
		return false
	}
	n.refreshedAt = time.Now()
	n.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var cs []contact
	if err := c.call(ctx, "listContacts", map[string]any{"allRecipients": true}, &cs); err != nil {
		logx.Warnf("listContacts failed: %v", err)
		return true
	}
	logx.Debugf("loaded %d contacts", len(cs))
	n.replace(cs)
	return true
}
