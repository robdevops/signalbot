package schedule

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// quotes may open or close the scheduled text: straight, and the curly ones
// phones type.
const quotes = "\"“”„"

// maxText caps the scheduled text, in runes: Signal's limit for one message.
const maxText = 2000

// exampleLayout is how the example time the help and the errors show is
// written: the current time, ready to copy and edit.
const exampleLayout = "2006-01-02 15:04"

// request is a new schedule as the user wrote it.
type request struct {
	text  string
	at    stamp  // its first time as written; for a repeat, maybe past
	start string // at, as it is saved
	every string // "" for a one-off
	group string // the group's name as typed; "" for the DM
}

// parse reads `"<text>" <time> [every <unit>] [group name]`, the time one
// word or a date and a time as two. It returns what is wrong, for the user,
// if it can't, showing example as a time.
func parse(args string, loc *time.Location, example string) (request, string) {
	var r request
	args = strings.TrimSpace(args)
	q, size := utf8.DecodeRuneInString(args)
	if !strings.ContainsRune(quotes, q) {
		return r, "Put the message in quotes, then the time: " + usage
	}
	body := args[size:]
	// The closing quote is the last one followed by a time, so the text
	// may hold quotes of its own. If none is, the last one that could be
	// a closing quote says what is wrong.
	problem := "The message needs a closing quote: " + usage
	seen := false
	for end := len(body); ; {
		j := strings.LastIndexAny(body[:end], quotes)
		if j < 0 {
			return r, problem
		}
		end = j
		_, qs := utf8.DecodeRuneInString(body[j:])
		rest := body[j+qs:]
		word, after := cutField(rest)
		dateOnly := false
		if isDate(word) { // a date and a time typed as two words
			if clock, rest := cutField(after); clock != "" && strings.Contains(clock, ":") {
				word, after = word+"T"+clock, rest
			} else {
				dateOnly = true
			}
		}
		switch {
		case word == "" && !seen:
			seen, problem = true, "Add a time after the message, like "+example+"."
		case word == "" || !startsWithSpace(rest):
		case dateOnly:
			if !seen {
				_, clock, _ := strings.Cut(example, " ")
				seen, problem = true, "Add a time after the date, like "+word+" "+clock+"."
			}
		default:
			at, start, ok := parseTime(word, loc)
			if !ok {
				if !seen {
					seen, problem = true, "I couldn't read the time "+word+". Write a date and time like "+example+
						", or with an offset, like "+example+"+10:00."
				}
				continue
			}
			r.text, r.at, r.start = strings.TrimSpace(body[:j]), at, start
			if word, rest := cutField(after); strings.EqualFold(word, "every") {
				unit, group := cutField(rest)
				if r.every = strings.ToLower(unit); units[r.every] == 0 {
					return r, "Say how often: every hour, day, week, month, quarter or year."
				}
				after = group
			}
			r.group = strings.TrimSpace(after)
			switch {
			case r.text == "":
				return r, "The message is empty."
			case utf8.RuneCountInString(r.text) > maxText:
				return r, "The message is too long: 2,000 characters at most."
			}
			return r, ""
		}
	}
}

// isDate reports whether s is a date alone, such as 2030-01-31.
func isDate(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// cutField splits s into its first word and the rest, both trimmed.
func cutField(s string) (string, string) {
	s = strings.TrimSpace(s)
	if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 {
		return s[:i], strings.TrimSpace(s[i:])
	}
	return s, ""
}

func startsWithSpace(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsSpace(r)
}

// plain is a group name with its emoji and symbols removed, its spaces
// collapsed, and in lower case, so "🏡 Family 🎉" is "family".
func plain(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r), unicode.IsNumber(r):
			b.WriteRune(r)
		case unicode.Is(unicode.Mn, r) && !unicode.Is(unicode.Variation_Selector, r): // an accent
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
