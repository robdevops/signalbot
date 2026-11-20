// Package geo is GeoGuess's game logic: reading a guess, scoring and ranking
// guesses, finding a street-level photo worth playing and naming its place.
// It knows nothing of MCP or of chats.
package geo

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"time"
)

// Point is a place on Earth in decimal degrees.
type Point struct {
	Lat, Lon float64
}

// valid reports whether p's latitude is in [-90, 90] and its longitude in
// [-180, 180].
func (p Point) valid() bool {
	return p.Lat >= -90 && p.Lat <= 90 && p.Lon >= -180 && p.Lon <= 180
}

// earthRadius is the mean radius of the Earth in km (IUGG).
const earthRadius = 6371.0088

// Km is the great-circle distance between a and b in km, by the haversine
// formula.
func Km(a, b Point) float64 {
	φ1, φ2 := radians(a.Lat), radians(b.Lat)
	dφ, dλ := φ2-φ1, radians(b.Lon-a.Lon)
	h := math.Pow(math.Sin(dφ/2), 2) + math.Cos(φ1)*math.Cos(φ2)*math.Pow(math.Sin(dλ/2), 2)
	return 2 * earthRadius * math.Asin(math.Sqrt(min(h, 1)))
}

func radians(deg float64) float64 { return deg * math.Pi / 180 }
func degrees(rad float64) float64 { return rad * 180 / math.Pi }

// MaxPoints is the score of a guess within 25 m.
const MaxPoints = 5000

// Points is the score of a guess km away: 5000·e^(−km/2000), rounded, and
// the full 5000 within 25 m.
func Points(km float64) int {
	if km <= 0.025 {
		return MaxPoints
	}
	return int(math.Round(MaxPoints * math.Exp(-km/2000)))
}

// Guess is one player's guess in a round: only how far off it was, never
// where it was.
type Guess struct {
	User string // the chat's pseudonymous user ID
	Km   float64
	At   time.Time
}

// Placing is a guess in the results: its rank (from 1) and its points.
type Placing struct {
	Guess
	Rank   int
	Points int
}

// Rank orders guesses by distance, nearest first; a tie goes to the earlier
// guess. Every placing has its own rank.
func Rank(guesses []Guess) []Placing {
	sorted := slices.Clone(guesses)
	slices.SortStableFunc(sorted, func(a, b Guess) int {
		return cmp.Or(cmp.Compare(a.Km, b.Km), a.At.Compare(b.At))
	})
	out := make([]Placing, len(sorted))
	for i, g := range sorted {
		out[i] = Placing{Guess: g, Rank: i + 1, Points: Points(g.Km)}
	}
	return out
}

// Distance is km as the results show it: metres under 1 km, one decimal under
// 100 km, else whole km with thousands separators.
func Distance(km float64) string {
	switch {
	case km < 1:
		// Rounding can reach 1000 m, which reads better as 1.0 km.
		if m := math.Round(km * 1000); m < 1000 {
			return strconv.Itoa(int(m)) + " m"
		}
		return "1.0 km"
	case math.Round(km*10)/10 < 100:
		return strconv.FormatFloat(km, 'f', 1, 64) + " km"
	}
	return Thousands(int(math.Round(km))) + " km"
}

// Thousands is n with a comma between each group of three digits.
func Thousands(n int) string {
	s := strconv.Itoa(n)
	sign := ""
	if s[0] == '-' {
		sign, s = "-", s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return sign + s
}
