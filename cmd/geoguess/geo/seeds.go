package geo

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
)

// seedsJSON lists regions with street-level coverage, each a centre, a
// radius and a weight, with the continents their countries are in and a cap
// on any one country's total weight.
//
//go:embed seeds.json
var seedsJSON []byte

// Seed is one region worth searching.
type Seed struct {
	CC       string  `json:"cc"` // ISO 3166-1 alpha-2
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	RadiusKm float64 `json:"radius_km"`
	Weight   float64 `json:"weight"`
	Name     string  `json:"name"` // for people reading the file
}

// Seeds are the regions a round's photo is looked for in, with the weights
// they are picked by after the per-country cap.
type Seeds struct {
	list    []Seed
	weights []float64 // cumulative
}

type seedFile struct {
	CountryCap float64             `json:"country_cap"`
	Continents map[string][]string `json:"continents"`
	Seeds      []Seed              `json:"seeds"`
}

// LoadSeeds is the embedded seeds, narrowed to regions: a comma-separated
// list of continents (europe, asia, africa, north-america, south-america,
// oceania) and ISO country codes, such as "europe" or "AU,NZ,JP". Empty is
// the whole world. A region with no seeds is an error.
func LoadSeeds(regions string) (*Seeds, error) {
	var f seedFile
	if err := json.Unmarshal(seedsJSON, &f); err != nil {
		return nil, fmt.Errorf("seeds.json: %w", err)
	}
	want, err := countries(f.Continents, regions)
	if err != nil {
		return nil, err
	}
	var list []Seed
	for _, s := range f.Seeds {
		if want == nil || want[s.CC] {
			list = append(list, s)
		}
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no seed regions in %q", regions)
	}
	return newSeeds(list, f.CountryCap), nil
}

// countries is the set of country codes regions names, or nil for all.
func countries(continents map[string][]string, regions string) (map[string]bool, error) {
	if strings.TrimSpace(regions) == "" {
		return nil, nil
	}
	want := map[string]bool{}
	for _, r := range strings.Split(regions, ",") {
		r = strings.TrimSpace(r)
		name := strings.NewReplacer(" ", "-", "_", "-").Replace(strings.ToLower(r))
		switch ccs, ok := continents[name]; {
		case ok:
			for _, cc := range ccs {
				want[cc] = true
			}
		case len(r) == 2:
			want[strings.ToUpper(r)] = true
		case r != "":
			return nil, fmt.Errorf("unknown region %q: use a continent (%s) or a two-letter country code", r, strings.Join(slices.Sorted(maps.Keys(continents)), ", "))
		}
	}
	return want, nil
}

// newSeeds weighs list, scaling down each country whose seeds weigh more
// than countryCap in all, so a few countries don't take most rounds.
func newSeeds(list []Seed, countryCap float64) *Seeds {
	total := map[string]float64{}
	for _, s := range list {
		total[s.CC] += s.Weight
	}
	s := &Seeds{list: list, weights: make([]float64, len(list))}
	sum := 0.0
	for i, seed := range list {
		w := seed.Weight
		if countryCap > 0 && total[seed.CC] > countryCap {
			w *= countryCap / total[seed.CC]
		}
		sum += w
		s.weights[i] = sum
	}
	return s
}

// Len is the number of seeds.
func (s *Seeds) Len() int { return len(s.list) }

// Has reports whether any seed is in the country cc (ISO 3166-1 alpha-2),
// so a photo found for one round can be matched to another's regions.
func (s *Seeds) Has(cc string) bool {
	return slices.ContainsFunc(s.list, func(seed Seed) bool { return seed.CC == cc })
}

// Pick is a seed chosen by weight.
func (s *Seeds) Pick(r *rand.Rand) Seed {
	x := r.Float64() * s.weights[len(s.weights)-1]
	i, _ := slices.BinarySearch(s.weights, x)
	return s.list[min(i, len(s.list)-1)]
}

// Point is a random point within the seed's radius, uniform over the disc.
func (s Seed) Point(r *rand.Rand) Point {
	d := s.RadiusKm * math.Sqrt(r.Float64()) / earthRadius // radians
	θ := 2 * math.Pi * r.Float64()
	lat := s.Lat + degrees(d*math.Cos(θ))
	lon := s.Lon + degrees(d*math.Sin(θ))/math.Max(math.Cos(radians(s.Lat)), 0.01)
	lat = math.Max(-89.9, math.Min(89.9, lat))
	lon = math.Mod(lon+540, 360) - 180
	return Point{Lat: lat, Lon: lon}
}

// boxHalf is half a search box's side in degrees: a 0.04° square is
// 0.0016 deg², under Mapillary's limit of 0.01.
const boxHalf = 0.02

// Around is the search box centred on p.
func Around(p Point) BBox {
	return BBox{
		MinLon: max(p.Lon-boxHalf, -180), MinLat: max(p.Lat-boxHalf, -90),
		MaxLon: min(p.Lon+boxHalf, 180), MaxLat: min(p.Lat+boxHalf, 90),
	}
}
