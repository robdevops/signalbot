package geo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Source is a place to find street-level photos: Mapillary or Panoramax.
type Source interface {
	// Name names the source in the credit and the log.
	Name() string
	// Candidates lists the photos in b.
	Candidates(ctx context.Context, b BBox) ([]Candidate, error)
	// Fetch downloads a candidate's photo.
	Fetch(ctx context.Context, c Candidate) ([]byte, error)
}

// BBox is an area to search, in decimal degrees.
type BBox struct {
	MinLon, MinLat, MaxLon, MaxLat float64
}

// String is the bbox as both APIs take it: minLon,minLat,maxLon,maxLat.
func (b BBox) String() string {
	return coord(b.MinLon) + "," + coord(b.MinLat) + "," + coord(b.MaxLon) + "," + coord(b.MaxLat)
}

// Centre is the middle of b.
func (b BBox) Centre() Point {
	return Point{Lat: (b.MinLat + b.MaxLat) / 2, Lon: (b.MinLon + b.MaxLon) / 2}
}

func coord(f float64) string { return strconv.FormatFloat(f, 'f', 6, 64) }

// Candidate is one photo a source offers, from its metadata alone.
type Candidate struct {
	ID       string // the source's own ID, which is all the state keeps
	Source   string // the source's Name
	Creator  string // the author, for the credit
	Licence  string // as the credit shows it, e.g. "CC BY-SA 4.0"
	Link     string // the photo's page, for the credit
	At       Point  // where it was taken: the answer
	Raw      Point  // the raw GPS position, for the drift check; At if there is none
	Captured time.Time
	Pano     bool    // an equirectangular 360° panorama
	Fisheye  bool    // a fisheye lens, which is refused
	Quality  float64 // 0–1; -1 if unknown (Mapillary's quality_score)
	Grade    int     // 1–5 (A is 5); 0 if unknown (Panoramax's grade)
	Width    int     // pixels; 0 if unknown
	Height   int
	Sequence string // the sequence the photo belongs to, if known

	image string // where to download the photo: a URL that may expire
}

// Key names c across sources, for the list of photos used recently.
func (c Candidate) Key() string { return c.Source + ":" + c.ID }

// StatusError is an API answering with an HTTP error status. It holds only
// the status, never the URL, so it is safe to log.
type StatusError struct {
	Source string
	Code   int
}

func (e StatusError) Error() string { return e.Source + ": status " + strconv.Itoa(e.Code) }

// ErrSlowDown is a source asking to be called less often; the caller should
// wait a while before searching again.
var ErrSlowDown = errors.New("rate limited")

// maxPhoto caps a downloaded photo.
const maxPhoto = 12 << 20

// userAgent names the program, never the person running it.
const userAgent = "geoguess (+https://github.com/robdevops/signalbot)"

// pacer spaces requests at least every apart.
type pacer struct {
	mu    sync.Mutex
	every time.Duration
	next  time.Time
}

// wait blocks until the next request may go, or ctx ends.
func (p *pacer) wait(ctx context.Context) error {
	p.mu.Lock()
	now := time.Now()
	at := now
	if p.next.After(now) {
		at = p.next
	}
	p.next = at.Add(p.every)
	p.mu.Unlock()
	t := time.NewTimer(at.Sub(now))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// request is one GET to an API or an image host.
type request struct {
	client *http.Client
	source string // names the source in errors
	url    string
	auth   string // the Authorization header, if any
	limit  int64  // the most bytes read
}

// get does r and returns the body. An error never holds the URL, which may
// carry a signature, nor the server's address.
func (r request) get(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: bad request", r.source)
	}
	req.Header.Set("User-Agent", userAgent)
	if r.auth != "" {
		req.Header.Set("Authorization", r.auth)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s: request failed", r.source)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, r.limit+1))
	if resp.StatusCode != http.StatusOK {
		return body, StatusError{Source: r.source, Code: resp.StatusCode}
	}
	if err != nil {
		return nil, fmt.Errorf("%s: reading the answer failed", r.source)
	}
	if int64(len(body)) > r.limit {
		return nil, fmt.Errorf("%s: answer too large", r.source)
	}
	return body, nil
}

// getJSON does r and decodes its JSON body into v.
func (r request) getJSON(ctx context.Context, v any) error {
	body, err := r.get(ctx)
	if err != nil {
		return err
	}
	if json.Unmarshal(body, v) != nil {
		return fmt.Errorf("%s: unreadable answer", r.source)
	}
	return nil
}

// geoJSONPoint is a GeoJSON point: [lon, lat].
type geoJSONPoint struct {
	Coordinates []float64 `json:"coordinates"`
}

func (g *geoJSONPoint) point() (Point, bool) {
	if g == nil || len(g.Coordinates) < 2 {
		return Point{}, false
	}
	p := Point{Lat: g.Coordinates[1], Lon: g.Coordinates[0]}
	return p, p.valid()
}
