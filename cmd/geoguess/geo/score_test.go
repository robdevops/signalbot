package geo

import (
	"math"
	"slices"
	"testing"
	"time"
)

func TestKm(t *testing.T) {
	for _, tc := range []struct {
		a, b Point
		want float64 // km, to within 0.1%
	}{
		{Point{-37.8136, 144.9631}, Point{-37.5622, 143.8503}, 101.8}, // Melbourne–Ballarat
		{Point{51.5074, -0.1278}, Point{48.8566, 2.3522}, 343.9},      // London–Paris
		{Point{0, 0}, Point{0, 180}, math.Pi * earthRadius},
		{Point{90, 0}, Point{-90, 0}, math.Pi * earthRadius},
		{Point{0, 179.9}, Point{0, -179.9}, 22.24},
		{Point{10, 10}, Point{10, 10}, 0},
	} {
		got := Km(tc.a, tc.b)
		if math.Abs(got-tc.want) > tc.want*0.001+1e-9 {
			t.Errorf("Km(%v, %v) = %.3f, want %.3f", tc.a, tc.b, got, tc.want)
		}
		if back := Km(tc.b, tc.a); math.Abs(back-got) > 1e-9 {
			t.Errorf("Km isn't symmetric: %v and %v", got, back)
		}
	}
}

func TestPoints(t *testing.T) {
	for _, tc := range []struct {
		km   float64
		want int
	}{
		{0, 5000}, {0.025, 5000}, {0.026, 5000}, {1, 4998}, {12, 4970},
		{640, 3631}, {2000, 1839}, {9812, 37}, {20015, 0},
	} {
		if got := Points(tc.km); got != tc.want {
			t.Errorf("Points(%v) = %d, want %d", tc.km, got, tc.want)
		}
	}
}

func TestRank(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 14, 30, 0, 0, time.UTC)
	guesses := []Guess{
		{User: "u_bob", Km: 640, At: t0.Add(10 * time.Second)},
		{User: "u_alice", Km: 12, At: t0.Add(50 * time.Second)},
		{User: "u_3", Km: 640, At: t0.Add(5 * time.Second)}, // ties with Bob, but earlier
		{User: "u_4", Km: 9812, At: t0},
	}
	got := Rank(guesses)
	want := []Placing{
		{Guess: guesses[1], Rank: 1, Points: 4970},
		{Guess: guesses[2], Rank: 2, Points: 3631},
		{Guess: guesses[0], Rank: 3, Points: 3631},
		{Guess: guesses[3], Rank: 4, Points: 37},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Rank = %+v\nwant %+v", got, want)
	}
	if guesses[0].User != "u_bob" {
		t.Error("Rank changed its input")
	}
	if len(Rank(nil)) != 0 {
		t.Error("Rank(nil) isn't empty")
	}
}

func TestDistance(t *testing.T) {
	for _, tc := range []struct {
		km   float64
		want string
	}{
		{0, "0 m"}, {0.0124, "12 m"}, {0.640, "640 m"}, {0.9996, "1.0 km"},
		{1, "1.0 km"}, {12.345, "12.3 km"}, {99.94, "99.9 km"}, {99.96, "100 km"},
		{640.4, "640 km"}, {9812.2, "9,812 km"}, {20015.1, "20,015 km"},
	} {
		if got := Distance(tc.km); got != tc.want {
			t.Errorf("Distance(%v) = %q, want %q", tc.km, got, tc.want)
		}
	}
}

func TestThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 37: "37", 999: "999", 1000: "1,000", 4970: "4,970", 1234567: "1,234,567", -1234: "-1,234"} {
		if got := Thousands(n); got != want {
			t.Errorf("Thousands(%d) = %q, want %q", n, got, want)
		}
	}
}
