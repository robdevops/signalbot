package linechart

import (
	"strings"
	"testing"
	"time"
)

func TestNiceTicks(t *testing.T) {
	ticks, dec := niceTicks(-41.2, 6.1, 5)
	if ticks[0] != -50 || ticks[len(ticks)-1] != 10 || dec != 0 {
		t.Fatalf("ticks %v decimals %d", ticks, dec)
	}
	if _, dec := niceTicks(0.11, 0.19, 5); dec != 2 {
		t.Fatalf("decimals %d, want 2", dec)
	}
}

func TestDateTicks(t *testing.T) {
	d := func(s string) time.Time { t, _ := time.Parse("2006-01-02", s); return t }
	labels := func(ts []dateTick) string {
		var out []string
		for _, tk := range ts {
			out = append(out, tk.label)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct{ from, to, want string }{
		{"2026-09-30", "2026-10-28", "1 Oct,15 Oct"},
		{"2026-10-01", "2026-10-04", "1 Oct,2 Oct,3 Oct,4 Oct"},
		{"2026-11-15", "2027-03-01", "Dec,Jan 2027,Feb,Mar"},
		{"2020-06-01", "2023-06-01", "2021,2022,2023"},
	} {
		if got := labels(dateTicks(d(tc.from), d(tc.to))); got != tc.want {
			t.Errorf("%s..%s: %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
	if n := len(dateTicks(d("2000-01-01"), d("2026-01-01"))); n > 8 {
		t.Errorf("%d year ticks, want at most 8", n)
	}
}

func TestSpreadLabels(t *testing.T) {
	got := spreadLabels([]float64{100, 105, 300}, 28, 0, 1000)
	if got[1]-got[0] < 28 || got[2] != 300 {
		t.Fatalf("got %v", got)
	}
	if got := spreadLabels([]float64{995, 999}, 28, 0, 1000); got[1] > 1000 || got[1]-got[0] < 28 {
		t.Fatalf("near the bottom: %v", got)
	}
}
