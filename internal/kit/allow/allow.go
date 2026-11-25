// Package allow reads a setting that says who may do something: everyone,
// no one, admins, and listed IDs, such as SIGNAL_DM_USER_IDS or
// SCHEDULE_USER_IDS.
package allow

import (
	"fmt"
	"strings"
)

// Policy is who may do something. An ID is listed if IDs has it; with All
// anyone may, and with Admins an admin may too, as the caller decides who
// is one.
type Policy struct {
	All    bool
	Admins bool
	IDs    map[string]bool
}

// Parse reads value: words separated by commas, semicolons or whitespace,
// each "all", "none", "admins" (any case) or an ID. Unset (empty) is
// "admins". "all" lets anyone, whatever else is listed; "none" adds no one,
// so "none" alone lets no one and "none, 123" only 123, not the admins.
func Parse(value string) Policy {
	words := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(words) == 0 {
		return Policy{Admins: true}
	}
	p := Policy{IDs: map[string]bool{}}
	for _, w := range words {
		switch strings.ToLower(w) {
		case "all":
			p.All = true
		case "admins":
			p.Admins = true
		case "none":
		default:
			p.IDs[w] = true
		}
	}
	return p
}

// Listed reports whether anyone may, or one of ids is listed: the answer
// needing no admin check.
func (p Policy) Listed(ids ...string) bool {
	if p.All {
		return true
	}
	for _, id := range ids {
		if id != "" && p.IDs[id] {
			return true
		}
	}
	return false
}

// Allows reports whether one of ids may: listed, or an admin if admins
// may, asking admin only then.
func (p Policy) Allows(admin func() bool, ids ...string) bool {
	return p.Listed(ids...) || p.Admins && admin != nil && admin()
}

// None reports whether no one may.
func (p Policy) None() bool { return !p.All && !p.Admins && len(p.IDs) == 0 }

// String says who may, for the startup log: "anyone", "no one", "admins",
// "admins and 2 IDs", "1 ID".
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
	switch n := len(p.IDs); n {
	case 0:
	case 1:
		parts = append(parts, "1 ID")
	default:
		parts = append(parts, fmt.Sprintf("%d IDs", n))
	}
	return strings.Join(parts, " and ")
}
