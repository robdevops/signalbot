package geo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixture is a recorded API answer from testdata, with {{cdn}} pointing at
// base. The recordings' people are renamed Alice and Bob, and their image
// URLs, which were signed, replaced.
func fixture(t *testing.T, name, base string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.ReplaceAll(string(data), "{{cdn}}", base+"/cdn"))
}

// fakeAPI is an httptest server whose handler the test sets, recording each
// request it gets.
type fakeAPI struct {
	*httptest.Server
	mu      sync.Mutex
	handle  func(w http.ResponseWriter, r *http.Request)
	queries []*http.Request
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.queries = append(f.queries, r.Clone(context.Background()))
		h := f.handle
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.queries...)
}

var testBox = BBox{MinLon: 143.85, MinLat: -37.57, MaxLon: 143.87, MaxLat: -37.55}

func TestMapillaryCandidates(t *testing.T) {
	api := newFakeAPI(t)
	api.handle = func(w http.ResponseWriter, r *http.Request) { w.Write(fixture(t, "mapillary_bbox.json", api.URL)) }
	m := NewMapillary(api.URL, "MLY|test", api.Client())
	cands, err := m.Candidates(context.Background(), testBox)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 8 {
		t.Fatalf("%d candidates, want 8", len(cands))
	}
	req := api.requests()[0]
	if got := req.Header.Get("Authorization"); got != "OAuth MLY|test" {
		t.Errorf("Authorization %q", got)
	}
	if req.Header.Get("User-Agent") != userAgent {
		t.Errorf("User-Agent %q", req.Header.Get("User-Agent"))
	}
	q := req.URL.Query()
	if req.URL.Path != "/images" || q.Get("bbox") != "143.850000,-37.570000,143.870000,-37.550000" || q.Get("limit") != "100" || !strings.Contains(q.Get("fields"), "computed_geometry") {
		t.Errorf("query %s %v", req.URL.Path, q)
	}

	flat, pano := cands[0], cands[6]
	want := Candidate{
		ID: "156004253082834", Source: "Mapillary", Creator: "alice", Licence: "CC BY-SA 4.0",
		Link:     "https://www.mapillary.com/app/?pKey=156004253082834",
		Captured: time.UnixMilli(1566687341409).UTC(), Fisheye: true, Quality: 0.612,
		Width: 3840, Height: 2160, Sequence: flat.Sequence, At: flat.At, Raw: flat.Raw,
		image: api.URL + "/cdn/156004253082834.jpg",
	}
	if flat != want {
		t.Errorf("candidate\n%+v\nwant\n%+v", flat, want)
	}
	if flat.At == flat.Raw || Km(flat.At, flat.Raw) > 0.05 || flat.At.Lat > -37.5 {
		t.Errorf("positions %v (computed) and %v (raw)", flat.At, flat.Raw)
	}
	if !pano.Pano || pano.Fisheye || pano.Creator != "bob" || pano.Quality != 0.775 {
		t.Errorf("panorama %+v", pano)
	}
}

func TestMapillaryImage(t *testing.T) {
	q := 0.5
	for _, tc := range []struct {
		name string
		im   mapillaryImage
		ok   bool
		want func(Candidate) bool
	}{
		{"quality null is unknown", mapillaryImage{ID: "1", Geometry: &geoJSONPoint{[]float64{1, 2}}}, true,
			func(c Candidate) bool { return c.Quality == -1 && c.At == (Point{2, 1}) && c.Raw == c.At }},
		{"quality kept", mapillaryImage{ID: "1", Geometry: &geoJSONPoint{[]float64{1, 2}}, QualityScore: &q}, true,
			func(c Candidate) bool { return c.Quality == 0.5 }},
		{"equirectangular is a panorama", mapillaryImage{ID: "1", Geometry: &geoJSONPoint{[]float64{1, 2}}, CameraType: "equirectangular"}, true,
			func(c Candidate) bool { return c.Pano }},
		{"spherical is a panorama", mapillaryImage{ID: "1", Geometry: &geoJSONPoint{[]float64{1, 2}}, CameraType: "spherical"}, true,
			func(c Candidate) bool { return c.Pano }},
		{"computed geometry is the answer", mapillaryImage{ID: "1", Geometry: &geoJSONPoint{[]float64{1, 2}}, ComputedGeometry: &geoJSONPoint{[]float64{3, 4}}}, true,
			func(c Candidate) bool { return c.At == (Point{4, 3}) && c.Raw == (Point{2, 1}) }},
		{"no capture time", mapillaryImage{ID: "1", ComputedGeometry: &geoJSONPoint{[]float64{3, 4}}}, true,
			func(c Candidate) bool { return c.Captured.IsZero() && c.Raw == c.At }},
		{"no position", mapillaryImage{ID: "1"}, false, nil},
		{"out of range", mapillaryImage{ID: "1", Geometry: &geoJSONPoint{[]float64{200, 2}}}, false, nil},
		{"no ID", mapillaryImage{Geometry: &geoJSONPoint{[]float64{1, 2}}}, false, nil},
	} {
		c, ok := tc.im.candidate()
		if ok != tc.ok || (ok && !tc.want(c)) {
			t.Errorf("%s: %+v, %v", tc.name, c, ok)
		}
	}
}

func TestMapillaryRetries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers []string // status and fixture, by request
		want    int      // candidates
		calls   int
		radius  bool // the last call was a radius search
		err     error
	}{
		{"empty is asked again", []string{"200 {\"data\":[]}", "200 mapillary_bbox.json"}, 8, 2, false, nil},
		{"empty twice", []string{"200 {\"data\":[]}", "200 {\"data\":[]}"}, 0, 2, false, nil},
		{"too dense falls back to a radius", []string{"500 mapillary_error.json", "200 mapillary_radius.json"}, 5, 2, true, nil},
		{"empty then too dense", []string{"200 {\"data\":[]}", "500 mapillary_error.json", "200 mapillary_radius.json"}, 5, 3, true, nil},
		{"rate limited", []string{`429 {"error":{"code":4,"message":"limit"}}`}, 0, 1, false, ErrSlowDown},
		{"rate limited as a 400", []string{`400 {"error":{"code":4,"message":"limit"}}`}, 0, 1, false, ErrSlowDown},
		{"refused", []string{`401 {"error":{"code":190}}`}, 0, 1, false, StatusError{Source: "Mapillary", Code: 401}},
		{"unreadable", []string{`200 <html>`}, 0, 1, false, errors.New("Mapillary: unreadable answer")},
	} {
		api := newFakeAPI(t)
		n := 0
		api.handle = func(w http.ResponseWriter, r *http.Request) {
			status, body, _ := strings.Cut(tc.answers[min(n, len(tc.answers)-1)], " ")
			n++
			if strings.HasSuffix(body, ".json") {
				body = string(fixture(t, body, api.URL))
			}
			code := map[string]int{"200": 200, "400": 400, "401": 401, "429": 429, "500": 500}[status]
			w.WriteHeader(code)
			w.Write([]byte(body))
		}
		cands, err := NewMapillary(api.URL, "MLY|test", api.Client()).Candidates(context.Background(), testBox)
		if (err == nil) != (tc.err == nil) || (err != nil && err.Error() != tc.err.Error()) {
			t.Errorf("%s: error %v, want %v", tc.name, err, tc.err)
		}
		reqs := api.requests()
		if len(cands) != tc.want || len(reqs) != tc.calls {
			t.Errorf("%s: %d candidates in %d calls, want %d in %d", tc.name, len(cands), len(reqs), tc.want, tc.calls)
			continue
		}
		last := reqs[len(reqs)-1].URL.Query()
		if tc.radius && (last.Get("lat") != "-37.560000" || last.Get("lng") != "143.860000" || last.Get("radius") != "50" || last.Has("bbox")) {
			t.Errorf("%s: radius search %v", tc.name, last)
		}
	}
}

func TestMapillaryFetch(t *testing.T) {
	api := newFakeAPI(t)
	expired := true
	api.handle = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/cdn/old.jpg" && expired:
			w.WriteHeader(http.StatusForbidden)
		case strings.HasPrefix(r.URL.Path, "/cdn/"):
			w.Write([]byte("jpeg " + r.URL.Path))
		case r.URL.Path == "/42" && r.URL.Query().Get("fields") == "thumb_2048_url" && r.Header.Get("Authorization") == "OAuth tok":
			w.Write([]byte(`{"id":"42","thumb_2048_url":"` + api.URL + `/cdn/fresh.jpg?sig=secret"}`))
		default:
			http.NotFound(w, r)
		}
	}
	m := NewMapillary(api.URL, "tok", api.Client())
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		image string
		want  string
	}{
		{"by the search's URL", api.URL + "/cdn/old.jpg", "jpeg /cdn/old.jpg"},
		{"no URL", "", "jpeg /cdn/fresh.jpg"},
	} {
		expired = false
		data, err := m.Fetch(ctx, Candidate{ID: "42", image: tc.image})
		if err != nil || string(data) != tc.want {
			t.Errorf("%s: %q, %v", tc.name, data, err)
		}
	}
	expired = true
	data, err := m.Fetch(ctx, Candidate{ID: "42", image: api.URL + "/cdn/old.jpg"})
	if err != nil || string(data) != "jpeg /cdn/fresh.jpg" {
		t.Errorf("expired URL: %q, %v", data, err)
	}
	// An error names neither the URL nor its signature.
	_, err = m.Fetch(ctx, Candidate{ID: "43", image: api.URL + "/cdn/old.jpg?sig=secret"})
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error %v", err)
	}
}

func TestRequestErrorsHoldNoURL(t *testing.T) {
	api := newFakeAPI(t)
	api.handle = func(w http.ResponseWriter, r *http.Request) {}
	addr := api.URL
	api.Close() // nothing listens now
	for _, url := range []string{addr + "/x?sig=secret", "://bad?sig=secret"} {
		_, err := request{client: http.DefaultClient, source: "Test", url: url, limit: 10}.get(context.Background())
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("error %v", err)
		}
	}
}

func TestPanoramaxCandidates(t *testing.T) {
	api := newFakeAPI(t)
	api.handle = func(w http.ResponseWriter, r *http.Request) { w.Write(fixture(t, "panoramax_search.json", api.URL)) }
	p := NewPanoramax(api.URL, api.Client())
	p.pace.every = 0
	cands, err := p.Candidates(context.Background(), testBox)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 5 {
		t.Fatalf("%d candidates, want 5", len(cands))
	}
	req := api.requests()[0]
	if req.URL.Path != "/api/search" || req.URL.Query().Get("bbox") != testBox.String() || req.Header.Get("User-Agent") != userAgent || req.Header.Get("Authorization") != "" {
		t.Errorf("request %v %v", req.URL, req.Header)
	}
	pano := cands[0]
	want := Candidate{
		ID: "9a479835-1331-4b8a-adce-f2173f61ffc8", Source: "Panoramax", Creator: "alice", Licence: "Etalab 2.0",
		Link:     api.URL + "/#focus=pic&pic=9a479835-1331-4b8a-adce-f2173f61ffc8",
		At:       Point{48.859925491, 2.345090985},
		Raw:      Point{48.859925491, 2.345090985},
		Captured: time.Date(2025, 5, 8, 9, 22, 36, 652e6, time.UTC),
		Pano:     true, Quality: -1, Grade: 5, Width: 12288, Height: 6144,
		Sequence: "600d5cee-5328-4dd4-8838-5f27ef85aa59",
		image:    api.URL + "/cdn/9a479835-1331-4b8a-adce-f2173f61ffc8/sd.jpg",
	}
	if pano != want {
		t.Errorf("candidate\n%+v\nwant\n%+v", pano, want)
	}
	flat := cands[3]
	if flat.Pano || flat.Licence != "CC BY-SA 4.0" || flat.Creator != "bob" || flat.Grade != 3 || flat.Width != 1920 {
		t.Errorf("flat %+v", flat)
	}

	data, err := p.Fetch(context.Background(), flat)
	if err != nil || !strings.Contains(string(data), "FeatureCollection") { // the fake serves the search for any path
		t.Errorf("fetch: %v", err)
	}
	if got := api.requests()[1].URL.Path; got != "/cdn/4ff8d96e-5114-4ed5-b593-daea754a6998/sd.jpg" {
		t.Errorf("fetched %s", got)
	}
}

func TestPanoramaxSkips(t *testing.T) {
	api := newFakeAPI(t)
	api.handle = func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"features":[
			{"id":"a","geometry":{"coordinates":[2,48]},"assets":{"sd":{"href":"x"}},"properties":{"license":"proprietary"}},
			{"id":"b","geometry":{"coordinates":[2,48]},"assets":{"hd":{"href":"x"}},"properties":{"license":"CC-BY-4.0"}},
			{"id":"c","assets":{"sd":{"href":"x"}},"properties":{"license":"CC-BY-4.0"}},
			{"id":"d","geometry":{"coordinates":[2,48]},"assets":{"sd":{"href":"x"}},"properties":{"license":"CC0-1.0","datetime":"bad"}}
		]}`))
	}
	p := NewPanoramax(api.URL, api.Client())
	p.pace.every = 0
	cands, err := p.Candidates(context.Background(), testBox)
	if err != nil || len(cands) != 1 || cands[0].ID != "d" || cands[0].Licence != "CC0 1.0" || !cands[0].Captured.IsZero() || cands[0].Grade != 0 {
		t.Errorf("%+v, %v", cands, err)
	}
	api.handle = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }
	if _, err := p.Candidates(context.Background(), testBox); err != ErrSlowDown {
		t.Errorf("429: %v", err)
	}
	api.handle = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }
	if _, err := p.Candidates(context.Background(), testBox); err != (StatusError{Source: "Panoramax", Code: 502}) {
		t.Errorf("502: %v", err)
	}
}

func TestGrade(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		pano              bool
		density, accuracy *float64
		want              int
	}{
		{false, nil, nil, 0},
		{false, f(5), nil, 1},
		{false, f(12), nil, 2},
		{false, f(20), nil, 3},
		{false, f(40), nil, 4},
		{true, f(10), nil, 3},
		{true, f(20), nil, 4},
		{true, f(34), nil, 5},
		{false, nil, f(0.3), 5},
		{false, nil, f(2), 4},
		{false, nil, f(5), 3},
		{false, nil, f(10), 2},
		{false, nil, f(11), 1},
		{true, f(34), f(2), 5},    // 0.8×5 + 0.2×4 = 4.8
		{false, f(20), f(3.6), 3}, // 0.8×3 + 0.2×3
		{false, f(12), f(0.3), 3}, // 0.8×2 + 0.2×5 = 2.6
		{false, f(12), f(11), 2},  // 0.8×2 + 0.2×1 = 1.8
		{false, f(5), f(0.3), 2},  // 0.8 + 1 = 1.8
		{false, f(40), f(11), 3},  // 3.2 + 0.2 = 3.4
		{true, f(10), f(20), 3},   // 2.4 + 0.2 = 2.6
	} {
		if got := grade(tc.pano, tc.density, tc.accuracy); got != tc.want {
			t.Errorf("grade(%v, %v, %v) = %d, want %d", tc.pano, deref(tc.density), deref(tc.accuracy), got, tc.want)
		}
	}
}

func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func TestLicenceName(t *testing.T) {
	for spdx, want := range map[string]string{
		"CC-BY-SA-4.0": "CC BY-SA 4.0", "CC-BY-4.0": "CC BY 4.0", "CC0-1.0": "CC0 1.0",
		"etalab-2.0": "Etalab 2.0", "": "", "proprietary": "", "CC-4.0": "", "MIT": "", "GPL-3.0": "",
	} {
		if got := licenceName(spdx); got != want {
			t.Errorf("licenceName(%q) = %q, want %q", spdx, got, want)
		}
	}
}

func TestPacer(t *testing.T) {
	p := &pacer{every: 30 * time.Millisecond}
	start := time.Now()
	for range 3 {
		if err := p.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Errorf("3 requests in %v, want at least 60ms", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.every = time.Hour
	p.wait(context.Background())
	if err := p.wait(ctx); err != context.Canceled {
		t.Errorf("cancelled wait: %v", err)
	}
}
