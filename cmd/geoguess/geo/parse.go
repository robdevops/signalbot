package geo

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Pattern is the watch pattern for a round: Go RE2 that catches a message
// that may hold a location. It is specific, so ordinary chatter doesn't
// match: a map URL's host, a geo: URI, or a decimal coordinate pair. Parse
// does the real reading, and a message it can't read isn't a guess.
const Pattern = `(?i)(?:\bmaps\.google\.[a-z.]+/|\bgoogle\.[a-z.]+/maps|\bmaps\.apple\.com\b|\bgeo:[+-]?\d|\bopenstreetmap\.org/|(?:^|[^\w.])-?\d{1,3}\.\d+°?\s*,\s*-?\d{1,3}\.\d+)`

// Parse reads the first location in text: a Google, Apple or OpenStreetMap
// map URL, a geo: URI (RFC 5870), or typed coordinates such as
// "-37.81, 144.96", each number with a decimal point. Only decimal degrees
// are read, and only a latitude in [-90, 90] with a longitude in
// [-180, 180]. Text that only looks like coordinates ("3, 4", times such as
// "12.30, 14.45", prices) isn't a location.
func Parse(text string) (Point, bool) {
	text = strings.ReplaceAll(text, "−", "-") // U+2212 MINUS SIGN
	for _, field := range strings.FieldsFunc(text, isFieldBreak) {
		field = strings.TrimRight(field, ".,;:!?")
		if p, ok := parseLink(field); ok {
			return p, true
		}
	}
	return parseTyped(text)
}

// isFieldBreak ends a link: white space, or a bracket or quote around one.
func isFieldBreak(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '<', '>', '(', ')', '[', ']', '"', '\'', '`':
		return true
	}
	return false
}

// parseLink reads a location from one map link or geo: URI.
func parseLink(s string) (Point, bool) {
	if len(s) > 4 && strings.EqualFold(s[:4], "geo:") {
		return parseGeoURI(s[4:])
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return Point{}, false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	switch {
	case isGoogle(host) && (strings.HasPrefix(host, "maps.") || strings.HasPrefix(u.Path, "/maps")):
		return parseGoogle(u)
	case host == "maps.apple.com":
		return firstParam(u.Query(), "ll", "coordinate", "q", "sll")
	case host == "openstreetmap.org":
		return parseOSM(u)
	}
	return Point{}, false
}

// googleHost is a Google host: google.com, google.com.au, maps.google.fr…
var googleHost = regexp.MustCompile(`^(?:maps\.)?google\.(?:[a-z]{2,3}|co|com)(?:\.[a-z]{2})?$`)

func isGoogle(host string) bool { return googleHost.MatchString(host) }

// googleAt is the map centre in a Google path: /maps/@-37.81,144.96,15z.
var googleAt = regexp.MustCompile(`/@([+-]?\d+(?:\.\d+)?),([+-]?\d+(?:\.\d+)?)(?:[,/]|$)`)

// parseGoogle reads q= (what Signal sends), ll= or query=, then a place or
// search path segment that is a coordinate pair, then the @ map centre.
func parseGoogle(u *url.URL) (Point, bool) {
	if p, ok := firstParam(u.Query(), "q", "ll", "query"); ok {
		return p, true
	}
	segs := strings.Split(u.Path, "/")
	for i, seg := range segs {
		if (seg == "place" || seg == "search") && i+1 < len(segs) {
			if v, err := url.PathUnescape(segs[i+1]); err == nil {
				if p, ok := parsePair(strings.ReplaceAll(v, "+", " ")); ok {
					return p, true
				}
			}
		}
	}
	if m := googleAt.FindStringSubmatch(u.Path); m != nil {
		return point(m[1], m[2])
	}
	return Point{}, false
}

// parseOSM reads a marker (mlat, mlon), else the map centre in the fragment
// (#map=15/-37.81/144.96).
func parseOSM(u *url.URL) (Point, bool) {
	q := u.Query()
	if q.Has("mlat") && q.Has("mlon") {
		return point(q.Get("mlat"), q.Get("mlon"))
	}
	frag, err := url.ParseQuery(u.Fragment)
	if err != nil {
		return Point{}, false
	}
	parts := strings.Split(frag.Get("map"), "/")
	if len(parts) != 3 {
		return Point{}, false
	}
	return point(parts[1], parts[2])
}

// parseGeoURI reads what follows "geo:": lat,lon[,alt][;params][?q=…].
// Android's geo:0,0?q=lat,lon(label) puts the place in q.
func parseGeoURI(s string) (Point, bool) {
	coords, query, _ := strings.Cut(s, "?")
	coords, _, _ = strings.Cut(coords, ";")
	parts := strings.Split(coords, ",")
	if len(parts) < 2 || len(parts) > 3 {
		return Point{}, false
	}
	p, ok := point(parts[0], parts[1])
	if ok && (p != Point{}) {
		return p, true
	}
	if q, err := url.ParseQuery(query); err == nil && q.Has("q") {
		v, _, _ := strings.Cut(q.Get("q"), "(")
		if qp, qok := parsePair(v); qok {
			return qp, true
		}
	}
	return p, ok
}

// firstParam is the first of the named query parameters that is a
// coordinate pair.
func firstParam(q url.Values, names ...string) (Point, bool) {
	for _, n := range names {
		if p, ok := parsePair(q.Get(n)); ok {
			return p, true
		}
	}
	return Point{}, false
}

// pair is a coordinate pair as a link holds it, maybe after "loc:".
var pair = regexp.MustCompile(`^\s*(?:loc:\s*)?([+-]?\d{1,3}(?:\.\d+)?)\s*,\s*([+-]?\d{1,3}(?:\.\d+)?)\s*$`)

func parsePair(s string) (Point, bool) {
	m := pair.FindStringSubmatch(s)
	if m == nil {
		return Point{}, false
	}
	return point(m[1], m[2])
}

// point is lat and lon as numbers, if both are in range.
func point(lat, lon string) (Point, bool) {
	φ, err1 := strconv.ParseFloat(lat, 64)
	λ, err2 := strconv.ParseFloat(lon, 64)
	p := Point{Lat: φ, Lon: λ}
	if err1 != nil || err2 != nil || !p.valid() {
		return Point{}, false
	}
	return p, true
}

// typed is a typed coordinate pair, each number with a decimal point, not
// part of a longer number, word, version ("v1.2.3") or link, nor next to a currency
// sign or a percent sign. A full stop may end the sentence after it.
var typed = regexp.MustCompile(`(?:^|[^\w.$€£¥=/@&?#+-])(-?\d{1,3}\.\d+)°?\s*,\s*(-?\d{1,3}\.\d+)°?(?:$|[^\w.%$€£¥]|\.(?:$|\D))`)

// parseTyped reads the first typed coordinate pair in text that isn't a
// pair of clock times or prices.
func parseTyped(text string) (Point, bool) {
	for _, m := range typed.FindAllStringSubmatchIndex(text, -1) {
		lat, lon := text[m[2]:m[3]], text[m[4]:m[5]]
		if (clock(lat) && clock(lon)) || money(text[:m[2]], text[m[5]:]) {
			continue
		}
		if p, ok := point(lat, lon); ok {
			return p, true
		}
	}
	return Point{}, false
}

// clock reports whether s reads as a time of day written with a dot, such
// as 9.30 or 14.45.
func clock(s string) bool {
	h, m, _ := strings.Cut(s, ".")
	if len(m) != 2 || len(h) > 2 || strings.HasPrefix(h, "-") {
		return false
	}
	hh, _ := strconv.Atoi(h)
	mm, _ := strconv.Atoi(m)
	return hh <= 23 && mm <= 59
}

// currencies are the words and signs that make a pair of numbers prices.
var currencies = []string{"$", "€", "£", "¥", "aud", "usd", "eur", "gbp", "nzd", "cad", "dollars", "dollar", "euros", "euro", "pounds", "bucks", "each"}

// money reports whether the text just before or after a pair names a
// currency, as in "AUD 3.50, 4.20" or "3.50, 4.20 euros".
func money(before, after string) bool {
	before = strings.ToLower(strings.TrimRight(before, " "))
	after = strings.ToLower(strings.TrimLeft(strings.TrimLeft(after, "°"), " "))
	for _, c := range currencies {
		sign := !isWordByte(c[0])
		if rest, ok := strings.CutSuffix(before, c); ok && (sign || rest == "" || !isWordByte(rest[len(rest)-1])) {
			return true
		}
		if rest, ok := strings.CutPrefix(after, c); ok && (sign || rest == "" || !isWordByte(rest[0])) {
			return true
		}
	}
	return false
}

func isWordByte(b byte) bool {
	return b == '_' || ('0' <= b && b <= '9') || ('a' <= b && b <= 'z')
}
