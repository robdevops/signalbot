// Package allow reads a setting that says who may do something: everyone,
// no one, admins, mods and listed IDs, such as SIGNAL_DM_USERS or
// SCHEDULE_ALLOWED_USERS.
package allow

import (
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

// Policy is who may do something. With All anyone may; an ID is listed if
// IDs has it, as Norm writes it; with Admins an admin may, and with Mods a
// moderator, as the network decides who is which.
type Policy struct {
	All, Admins, Mods bool
	IDs               map[string]bool
}

// Parse reads value: words separated by commas, semicolons or whitespace,
// each "all", "none", "admins", "mods" (any case) or an ID. Unset (empty)
// is "admins, mods". "all" lets anyone, whatever else is listed; "none"
// adds no one, so "none" alone lets no one and "none, 123" only 123.
func Parse(value string) Policy {
	words := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(words) == 0 {
		return Policy{Admins: true, Mods: true}
	}
	p := Policy{IDs: map[string]bool{}}
	for _, w := range words {
		switch strings.ToLower(w) {
		case "all":
			p.All = true
		case "admins":
			p.Admins = true
		case "mods":
			p.Mods = true
		case "none":
		default:
			p.IDs[Norm(w)] = true
		}
	}
	return p
}

var (
	snowflake = regexp.MustCompile(`^[0-9]{17,20}$`)       // a Discord ID; Telegram's are shorter
	slackUser = regexp.MustCompile(`^[UW][A-Z0-9]{8,20}$`) // a Slack member ID
)

// Norm is id as the bot writes a sender: a Signal UUID or number and a
// Telegram ID bare, a Discord ID with "discord:", a Slack one with
// "slack:". An ID may be written with its network's prefix or without:
// "signal:" and "telegram:" are dropped, and a bare number of 17 digits or
// more is Discord's, a bare U… or W… member ID Slack's.
func Norm(id string) string {
	if rest, ok := strings.CutPrefix(id, "signal:"); ok {
		return rest
	}
	if rest, ok := strings.CutPrefix(id, "telegram:"); ok {
		return rest
	}
	switch {
	case snowflake.MatchString(id):
		return "discord:" + id
	case slackUser.MatchString(id):
		return "slack:" + id
	}
	return id
}

// Listed reports whether anyone may, or one of ids is listed: the answer
// needing no lookup.
func (p Policy) Listed(ids ...string) bool {
	if p.All {
		return true
	}
	for _, id := range ids {
		if id != "" && p.IDs[Norm(id)] {
			return true
		}
	}
	return false
}

// Roles reports whether admins or mods may, so a network need ask whether
// someone is one.
func (p Policy) Roles() bool { return !p.All && (p.Admins || p.Mods) }

// Allows reports whether one of ids may: listed, or, if admins or mods
// may, one by role, asking role only then.
func (p Policy) Allows(role func() (admin, mod bool), ids ...string) bool {
	if p.Listed(ids...) {
		return true
	}
	if !p.Roles() || role == nil {
		return false
	}
	admin, mod := role()
	return p.Admins && admin || p.Mods && mod
}

// None reports whether no one may.
func (p Policy) None() bool { return !p.All && !p.Admins && !p.Mods && len(p.IDs) == 0 }

// Who names those who may by role, for a refusal: "admins", "mods" or
// "admins and mods"; "" for none.
func (p Policy) Who() string {
	switch {
	case p.Admins && p.Mods:
		return "admins and mods"
	case p.Admins:
		return "admins"
	case p.Mods:
		return "mods"
	}
	return ""
}

// String says who may, for the startup log: "anyone", "no one", "admins
// and mods", "admins, mods and 2 IDs", "1 ID".
func (p Policy) String() string {
	switch {
	case p.All:
		return "anyone"
	case p.None():
		return "no one"
	}
	var parts []string
	if p.Admins {
		parts = append(parts, "admins")
	}
	if p.Mods {
		parts = append(parts, "mods")
	}
	switch n := len(p.IDs); n {
	case 0:
	case 1:
		parts = append(parts, "1 ID")
	default:
		parts = append(parts, strconv.Itoa(n)+" IDs")
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// Refused logs DMs a setting refuses: a warning naming the sender's ID and
// the setting to add it to, once per sender, for at most 10,000 senders a
// run, so a stranger's every message doesn't fill the log.
type Refused struct {
	Setting string // the setting that refused them, such as SIGNAL_DM_USERS
	L       logx.L // the log warned in, the network's ("Signal: ")

	mu   sync.Mutex
	seen map[string]bool
}

// Log logs id's refused DM, the first time.
func (r *Refused) Log(id string) {
	r.mu.Lock()
	first := !r.seen[id] && len(r.seen) < 10000
	if first {
		if r.seen == nil {
			r.seen = map[string]bool{}
		}
		r.seen[id] = true
	}
	r.mu.Unlock()
	if first {
		r.L.Warnf("a DM from %s ignored: %s doesn't allow it; add the ID to %s to allow DMs from them", id, r.Setting, r.Setting)
	}
}
