package geo

import (
	"math"
	"regexp"
	"testing"
)

func TestParse(t *testing.T) {
	mel := Point{-37.8136, 144.9631}
	for _, tc := range []struct {
		text string
		want Point
		ok   bool
	}{
		// Signal's + → Location.
		{"https://maps.google.com/maps?q=-37.8136%2C144.9631", mel, true},
		{"https://maps.google.com/maps?q=-37.8136,144.9631", mel, true},
		{"Here: https://maps.google.com/maps?q=-37.8136%2C144.9631.", mel, true},
		{"maps.google.com/maps?q=-37.8136%2C%20144.9631", mel, true},
		{"https://www.google.com/maps?q=loc:-37.8136,144.9631", mel, true},
		{"https://www.google.com.au/maps?ll=-37.8136,144.9631&z=15", mel, true},
		{"https://www.google.com/maps/search/?api=1&query=-37.8136%2C144.9631", mel, true},
		{"google.com/maps/@-37.8136,144.9631,15z", mel, true},
		{"https://www.google.com/maps/place/Melbourne+VIC/@-37.8136,144.9631,12z/data=!3m1", mel, true},
		{"https://www.google.com/maps/place/-37.8136,+144.9631", mel, true},
		{"(https://maps.google.fr/?q=-37.8136,144.9631)", mel, true},
		// Apple.
		{"https://maps.apple.com/?ll=-37.8136,144.9631&q=Pin", mel, true},
		{"https://maps.apple.com/?q=Cafe&sll=-37.8136,144.9631", mel, true},
		{"https://maps.apple.com/place?coordinate=-37.8136,144.9631", mel, true},
		// geo: URIs.
		{"geo:-37.8136,144.9631", mel, true},
		{"GEO:-37.8136,144.9631,30;u=35", mel, true},
		{"geo:0,0?q=-37.8136,144.9631(Pin)", mel, true},
		{"geo:0,0", Point{}, true},
		// OpenStreetMap.
		{"https://www.openstreetmap.org/#map=15/-37.8136/144.9631", mel, true},
		{"https://www.openstreetmap.org/#map=15/-37.8136/144.9631&layers=H", mel, true},
		{"https://www.openstreetmap.org/?mlat=-37.8136&mlon=144.9631#map=3/0/0", mel, true},
		// Typed.
		{"-37.8136, 144.9631", mel, true},
		{"-37.8136,144.9631", mel, true},
		{"I reckon −37.8136 , 144.9631.", mel, true},
		{"-37.8136°, 144.9631°", mel, true},
		{"my guess is 48.8584, 2.2945 final answer", Point{48.8584, 2.2945}, true},
		{"0.0, 0.0", Point{}, true},
		{"90.0, -180.0", Point{90, -180}, true},

		// Not locations.
		{"3, 4", Point{}, false},
		{"-37, 144", Point{}, false},
		{"meet at 12.30, 14.45 or later", Point{}, false},
		{"9.30, 10.15", Point{}, false},
		{"$3.50, $4.20", Point{}, false},
		{"they were 3.50, 4.20 each", Point{}, false},
		{"AUD 3.50, 4.20", Point{}, false},
		{"3.50, 4.20 euros", Point{}, false},
		{"3.5%, 4.2%", Point{}, false},
		{"v1.2, v1.3", Point{}, false},
		{"versions 1.2.3, 1.2.4", Point{}, false},
		{"91.5, 10.0", Point{}, false},
		{"45.5, 180.5", Point{}, false},
		{"https://maps.google.com/maps?q=Melbourne", Point{}, false},
		{"https://maps.google.com/maps?q=-97.1,144.9", Point{}, false},
		{"https://example.com/maps?q=-37.8136,144.9631", Point{}, false},
		{"https://maps.google.com.evil.example/?q=-37.8136,144.9631", Point{}, false},
		{"https://www.google.com/search?q=-37.8136,144.9631", Point{}, false},
		{"https://maps.app.goo.gl/abc123", Point{}, false},
		{"geo: what a game", Point{}, false},
		{"geo:-37.8136", Point{}, false},
		{"https://www.openstreetmap.org/#map=15/-37.8136", Point{}, false},
		{"", Point{}, false},
	} {
		got, ok := Parse(tc.text)
		if ok != tc.ok || !near(got, tc.want) {
			t.Errorf("Parse(%q) = %v, %v; want %v, %v", tc.text, got, ok, tc.want, tc.ok)
		}
	}
}

func near(a, b Point) bool {
	return math.Abs(a.Lat-b.Lat) < 1e-9 && math.Abs(a.Lon-b.Lon) < 1e-9
}

// TestPatternCatchesEveryForm checks the watch pattern catches every text
// Parse reads, so no guess is missed.
func TestPatternCatchesEveryForm(t *testing.T) {
	re := regexp.MustCompile(Pattern)
	for _, text := range []string{
		"https://maps.google.com/maps?q=-37.8136%2C144.9631",
		"google.com/maps/@-37.81,144.96,15z",
		"https://www.google.com.au/maps/place/X/@-37.81,144.96,12z",
		"https://maps.apple.com/?ll=-37.81,144.96",
		"https://maps.apple.com?q=x&sll=-37.81,144.96",
		"geo:-37.81,144.96",
		"geo:0,0?q=-37.81,144.96",
		"https://www.openstreetmap.org/#map=15/-37.81/144.96",
		"https://www.openstreetmap.org/?mlat=-37.81&mlon=144.96",
		"-37.81, 144.96",
		"guess: 48.85,2.29",
		"-37.81°, 144.96°",
	} {
		if !re.MatchString(text) {
			t.Errorf("pattern misses %q", text)
		}
		if _, ok := Parse(text); !ok {
			t.Errorf("Parse misses %q", text)
		}
	}
}

// TestPatternSkipsChatter checks the watch pattern is specific: ordinary
// chat lines, links and numbers don't match, so it passes signalbot's
// catch-all check.
func TestPatternSkipsChatter(t *testing.T) {
	re := regexp.MustCompile(Pattern)
	if len(Pattern) > 1024 {
		t.Errorf("pattern is %d bytes, over the 1 KB limit", len(Pattern))
	}
	for _, text := range []string{
		"", "lol", "ok", "see you at 5", "what's for dinner?", "running 10 min late",
		"meet at 12:30, 14:45 is too late", "3, 4", "it cost 3.50", "1.5 litres, 2 eggs",
		"version 1.2.3 is out", "the score was 3-4", "call me on 0412 345 678",
		"https://www.abc.net.au/news/2024-05-01/some-story/103789456",
		"https://www.google.com/search?q=weather", "https://youtu.be/dQw4w9WgXcQ",
		"Alice: are we still on for Saturday?", "Bob, can you grab milk",
		"I'm at the station", "we're in Melbourne now", "geography homework is due",
		"lat long of the house?", "😂😂😂", "100%", "1,000,000", "pi is 3.14159",
		"Google Maps says 20 min", "check apple maps", "openstreetmap is great",
	} {
		if re.MatchString(text) {
			t.Errorf("pattern matches chatter %q", text)
		}
	}
}
