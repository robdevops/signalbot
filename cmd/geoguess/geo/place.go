package geo

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NominatimURL is OpenStreetMap's public Nominatim, whose policy allows at
// most one request a second, with a User-Agent that names the program.
const NominatimURL = "https://nominatim.openstreetmap.org"

// Place is where a photo was taken, as the reveal names it.
type Place struct {
	Name        string // "Ballarat, Victoria, Australia"
	Country     string
	CountryCode string // ISO 3166-1 alpha-2, upper case
}

// Nominatim names places by reverse geocoding, at most once a second.
type Nominatim struct {
	url    string
	client *http.Client
	pace   *pacer
}

// NewNominatim is a client for the Nominatim at base (NominatimURL, or a
// self-hosted one).
func NewNominatim(base string, client *http.Client) *Nominatim {
	return &Nominatim{url: strings.TrimRight(base, "/"), client: client, pace: &pacer{every: time.Second}}
}

// Reverse names the town, region and country at p. A place with no country,
// such as the open sea, is RejectNoPlace.
func (n *Nominatim) Reverse(ctx context.Context, p Point) (Place, error) {
	if err := n.pace.wait(ctx); err != nil {
		return Place{}, err
	}
	q := url.Values{
		"format": {"jsonv2"}, "lat": {coord(p.Lat)}, "lon": {coord(p.Lon)},
		"zoom": {"10"}, "addressdetails": {"1"}, "accept-language": {"en"},
	}
	var a struct {
		Error   string            `json:"error"`
		Address map[string]string `json:"address"`
	}
	r := request{client: n.client, source: "Nominatim", url: n.url + "/reverse?" + q.Encode(), limit: 1 << 20}
	if err := r.getJSON(ctx, &a); err != nil {
		return Place{}, err
	}
	country := a.Address["country"]
	if a.Error != "" || country == "" {
		return Place{}, RejectNoPlace
	}
	parts := []string{}
	add := func(s string) {
		if s != "" && (len(parts) == 0 || parts[len(parts)-1] != s) {
			parts = append(parts, s)
		}
	}
	add(first(a.Address, "city", "town", "village", "municipality", "hamlet", "suburb", "county"))
	add(first(a.Address, "state", "region", "province", "state_district"))
	add(country)
	return Place{
		Name:        strings.Join(parts, ", "),
		Country:     country,
		CountryCode: strings.ToUpper(a.Address["country_code"]),
	}, nil
}

// first is the first of keys that address has.
func first(address map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := address[k]; v != "" {
			return v
		}
	}
	return ""
}
