package geo

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestSeedsFile(t *testing.T) {
	var f seedFile
	if err := json.Unmarshal(seedsJSON, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Seeds) < 150 || f.CountryCap <= 0 {
		t.Errorf("%d seeds, cap %v", len(f.Seeds), f.CountryCap)
	}
	continent := map[string]string{}
	for name, ccs := range f.Continents {
		for _, cc := range ccs {
			if other, ok := continent[cc]; ok {
				t.Errorf("%s is in %s and %s", cc, other, name)
			}
			continent[cc] = name
		}
	}
	countries := map[string]bool{}
	for _, s := range f.Seeds {
		countries[s.CC] = true
		if _, ok := continent[s.CC]; !ok {
			t.Errorf("%s (%s) is in no continent", s.CC, s.Name)
		}
		if !(Point{s.Lat, s.Lon}).valid() || s.RadiusKm <= 0 || s.RadiusKm > 100 || s.Weight <= 0 || s.Name == "" {
			t.Errorf("seed %+v", s)
		}
	}
	if len(countries) < 60 || len(f.Continents) != 6 {
		t.Errorf("%d countries in %d continents", len(countries), len(f.Continents))
	}
}

func TestLoadSeeds(t *testing.T) {
	world, err := LoadSeeds("")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		regions string
		want    func(Seed) bool
		err     string
	}{
		{"europe", func(s Seed) bool { return s.Lon > -30 && s.Lon < 45 && s.Lat > 27 }, ""},
		{"AU,nz, JP", func(s Seed) bool { return s.CC == "AU" || s.CC == "NZ" || s.CC == "JP" }, ""},
		{"South America", func(s Seed) bool { return s.Lat < 13 && s.Lon < -34 && s.Lon > -82 }, ""},
		{"oceania,FR", func(s Seed) bool { return s.CC == "AU" || s.CC == "NZ" || s.CC == "FR" }, ""},
		{"atlantis", nil, "unknown region"},
		{"XX", nil, "no seed regions"},
	} {
		s, err := LoadSeeds(tc.regions)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%q: %v, want %q", tc.regions, err, tc.err)
			}
			continue
		}
		if err != nil || s.Len() == 0 || s.Len() >= world.Len() {
			t.Errorf("%q: %d seeds, %v", tc.regions, s.Len(), err)
			continue
		}
		for _, seed := range s.list {
			if !tc.want(seed) {
				t.Errorf("%q: has %+v", tc.regions, seed)
			}
		}
	}
}

func TestSeedsCapCountries(t *testing.T) {
	list := []Seed{
		{CC: "US", Weight: 6}, {CC: "US", Weight: 6}, // 12, capped at 4
		{CC: "NZ", Weight: 2}, {CC: "AU", Weight: 4},
	}
	s := newSeeds(list, 4)
	r := rand.New(rand.NewPCG(1, 1))
	got := map[string]int{}
	const n = 20000
	for range n {
		got[s.Pick(r).CC]++
	}
	for cc, share := range map[string]float64{"US": 0.4, "NZ": 0.2, "AU": 0.4} {
		if f := float64(got[cc]) / n; math.Abs(f-share) > 0.02 {
			t.Errorf("%s picked %.3f of the time, want %.2f", cc, f, share)
		}
	}

	world, _ := LoadSeeds("")
	us := 0
	for range n {
		if world.Pick(r).CC == "US" {
			us++
		}
	}
	if f := float64(us) / n; f > 0.05 {
		t.Errorf("the US takes %.3f of rounds", f)
	}
}

func TestSeedPoint(t *testing.T) {
	r := rand.New(rand.NewPCG(2, 2))
	for _, s := range []Seed{
		{Lat: -37.81, Lon: 144.96, RadiusKm: 40},
		{Lat: 69.65, Lon: 18.96, RadiusKm: 15},
		{Lat: 0, Lon: 179.99, RadiusKm: 30}, // wraps round the antimeridian
	} {
		far := 0.0
		for range 2000 {
			p := s.Point(r)
			if !p.valid() {
				t.Fatalf("%v is out of range", p)
			}
			d := Km(p, Point{s.Lat, s.Lon})
			if d > s.RadiusKm*1.001 {
				t.Errorf("%v is %.1f km from %+v", p, d, s)
			}
			far = max(far, d)
		}
		if far < s.RadiusKm*0.9 {
			t.Errorf("points stay within %.1f of %v km", far, s.RadiusKm)
		}
	}
}

func TestAround(t *testing.T) {
	for _, p := range []Point{{-37.81, 144.96}, {89.99, 0}, {0, -180}} {
		b := Around(p)
		area := (b.MaxLon - b.MinLon) * (b.MaxLat - b.MinLat)
		if area >= 0.01 || area <= 0 || b.MinLon < -180 || b.MaxLat > 90 {
			t.Errorf("Around(%v) = %+v, %.4f deg²", p, b, area)
		}
	}
	if got := Around(Point{-37.81, 144.96}).String(); got != "144.940000,-37.830000,144.980000,-37.790000" {
		t.Errorf("String %s", got)
	}
}

func TestSeedsHas(t *testing.T) {
	europe, err := LoadSeeds("europe")
	if err != nil {
		t.Fatal(err)
	}
	if !europe.Has("FR") || europe.Has("AU") || europe.Has("") {
		t.Error("europe's countries")
	}
}
