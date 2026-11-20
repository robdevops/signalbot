package geo

import (
	"context"
	"net/http"
	"testing"
)

func TestNominatimReverse(t *testing.T) {
	api := newFakeAPI(t)
	answer := ""
	status := http.StatusOK
	api.handle = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(answer))
	}
	n := NewNominatim(api.URL+"/", api.Client())
	n.pace.every = 0
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		answer string
		want   Place
		err    error
	}{
		{"city", `{"display_name":"…","address":{"city":"Ballarat","state":"Victoria","country":"Australia","country_code":"au"}}`,
			Place{Name: "Ballarat, Victoria, Australia", Country: "Australia", CountryCode: "AU"}, nil},
		{"village and county", `{"address":{"village":"Bob's Bay","county":"Some County","country":"Ireland","country_code":"ie"}}`,
			Place{Name: "Bob's Bay, Ireland", Country: "Ireland", CountryCode: "IE"}, nil},
		{"county only", `{"address":{"county":"Alice County","state":"Ohio","country":"United States","country_code":"us"}}`,
			Place{Name: "Alice County, Ohio, United States", Country: "United States", CountryCode: "US"}, nil},
		{"city-state", `{"address":{"city":"Singapore","country":"Singapore","country_code":"sg"}}`,
			Place{Name: "Singapore", Country: "Singapore", CountryCode: "SG"}, nil},
		{"no country", `{"address":{"state":"International waters"}}`, Place{}, RejectNoPlace},
		{"open sea", `{"error":"Unable to geocode"}`, Place{}, RejectNoPlace},
	} {
		answer = tc.answer
		got, err := n.Reverse(ctx, Point{-37.5622, 143.8503})
		if got != tc.want || err != tc.err {
			t.Errorf("%s: %+v, %v; want %+v, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
	req := api.requests()[0]
	q := req.URL.Query()
	if req.URL.Path != "/reverse" || q.Get("lat") != "-37.562200" || q.Get("lon") != "143.850300" || q.Get("zoom") != "10" || q.Get("format") != "jsonv2" {
		t.Errorf("request %v", req.URL)
	}
	if req.Header.Get("User-Agent") != userAgent {
		t.Errorf("User-Agent %q", req.Header.Get("User-Agent"))
	}

	status = http.StatusServiceUnavailable
	if _, err := n.Reverse(ctx, Point{}); err != (StatusError{Source: "Nominatim", Code: 503}) {
		t.Errorf("503: %v", err)
	}
}
