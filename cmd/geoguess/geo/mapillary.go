package geo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// MapillaryURL is Mapillary's Graph API.
const MapillaryURL = "https://graph.mapillary.com"

// mapillaryFields are the fields a search asks for.
const mapillaryFields = "id,computed_geometry,geometry,captured_at,camera_type,is_pano,quality_score,width,height,sequence,creator,thumb_2048_url"

// mapillaryLimit is the most images one search returns.
const mapillaryLimit = 100

// maxMapillaryAnswer caps one search's answer.
const maxMapillaryAnswer = 4 << 20

// Mapillary searches Mapillary's Graph API v4. Its photos are CC BY-SA 4.0.
type Mapillary struct {
	url    string
	token  string
	client *http.Client
}

// NewMapillary is a Mapillary client for the API at base (MapillaryURL),
// sending token (MLY|…) as "Authorization: OAuth <token>": Mapillary's
// answers to a Bearer token hold no data.
func NewMapillary(base, token string, client *http.Client) *Mapillary {
	return &Mapillary{url: base, token: token, client: client}
}

// Name is "Mapillary".
func (m *Mapillary) Name() string { return "Mapillary" }

type mapillaryImage struct {
	ID               string        `json:"id"`
	ComputedGeometry *geoJSONPoint `json:"computed_geometry"`
	Geometry         *geoJSONPoint `json:"geometry"`
	CapturedAt       int64         `json:"captured_at"` // ms since the epoch, by the camera's clock
	CameraType       string        `json:"camera_type"`
	IsPano           bool          `json:"is_pano"`
	QualityScore     *float64      `json:"quality_score"` // null is unknown, not bad
	Width            int           `json:"width"`
	Height           int           `json:"height"`
	Sequence         string        `json:"sequence"`
	Creator          struct {
		Username string `json:"username"`
	} `json:"creator"`
	Thumb string `json:"thumb_2048_url"`
}

type mapillaryAnswer struct {
	Data  []mapillaryImage `json:"data"`
	Error *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// Candidates searches b, which must be under 0.01 deg². An empty answer is
// asked again once, since Mapillary sometimes leaves out images it has; a
// server error (Mapillary's answer to an area too dense to list) falls back
// to a radius search around b's centre.
func (m *Mapillary) Candidates(ctx context.Context, b BBox) ([]Candidate, error) {
	q := url.Values{"bbox": {b.String()}, "fields": {mapillaryFields}, "limit": {strconv.Itoa(mapillaryLimit)}}
	images, err := m.search(ctx, q)
	if err == nil && len(images) == 0 {
		images, err = m.search(ctx, q)
	}
	if se := (StatusError{}); errors.As(err, &se) && se.Code >= 500 {
		c := b.Centre()
		images, err = m.search(ctx, url.Values{
			"lat": {coord(c.Lat)}, "lng": {coord(c.Lon)}, "radius": {"50"},
			"fields": {mapillaryFields}, "limit": {strconv.Itoa(mapillaryLimit)},
		})
	}
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(images))
	for _, im := range images {
		if c, ok := im.candidate(); ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// search is one call to /images.
func (m *Mapillary) search(ctx context.Context, q url.Values) ([]mapillaryImage, error) {
	r := request{client: m.client, source: m.Name(), url: m.url + "/images?" + q.Encode(), auth: "OAuth " + m.token, limit: maxMapillaryAnswer}
	body, err := r.get(ctx)
	var a mapillaryAnswer
	jerr := json.Unmarshal(body, &a)
	if a.Error != nil && a.Error.Code == 4 {
		return nil, ErrSlowDown // "application request limit reached"
	}
	if err != nil {
		return nil, err
	}
	if jerr != nil || a.Error != nil {
		return nil, fmt.Errorf("%s: unreadable answer", m.Name())
	}
	return a.Data, nil
}

// candidate is im as a Candidate, if it has an ID and a position.
func (im mapillaryImage) candidate() (Candidate, bool) {
	raw, rawOK := im.Geometry.point()
	at, ok := im.ComputedGeometry.point()
	if !ok {
		at, ok = raw, rawOK
	}
	if !rawOK {
		raw = at
	}
	if im.ID == "" || !ok {
		return Candidate{}, false
	}
	c := Candidate{
		ID:       im.ID,
		Source:   "Mapillary",
		Creator:  im.Creator.Username,
		Licence:  "CC BY-SA 4.0",
		Link:     "https://www.mapillary.com/app/?pKey=" + url.QueryEscape(im.ID),
		At:       at,
		Raw:      raw,
		Pano:     im.IsPano || im.CameraType == "spherical" || im.CameraType == "equirectangular",
		Fisheye:  im.CameraType == "fisheye",
		Quality:  -1,
		Width:    im.Width,
		Height:   im.Height,
		Sequence: im.Sequence,
		image:    im.Thumb,
	}
	if im.CapturedAt > 0 {
		c.Captured = time.UnixMilli(im.CapturedAt).UTC()
	}
	if im.QualityScore != nil {
		c.Quality = *im.QualityScore
	}
	return c, true
}

// Fetch downloads c's 2048-px thumbnail. The URL a search gave is signed and
// expires, so a candidate without one, or whose URL has expired, gets a fresh
// one by ID.
func (m *Mapillary) Fetch(ctx context.Context, c Candidate) ([]byte, error) {
	if c.image != "" {
		data, err := request{client: m.client, source: m.Name(), url: c.image, limit: maxPhoto}.get(ctx)
		if err == nil {
			return data, nil
		}
		if se := (StatusError{}); !errors.As(err, &se) || (se.Code != http.StatusForbidden && se.Code != http.StatusNotFound && se.Code != http.StatusGone) {
			return nil, err
		}
	}
	var fresh struct {
		Thumb string `json:"thumb_2048_url"`
	}
	q := url.Values{"fields": {"thumb_2048_url"}}
	r := request{client: m.client, source: m.Name(), url: m.url + "/" + url.PathEscape(c.ID) + "?" + q.Encode(), auth: "OAuth " + m.token, limit: 64 << 10}
	if err := r.getJSON(ctx, &fresh); err != nil {
		return nil, err
	}
	if fresh.Thumb == "" {
		return nil, fmt.Errorf("%s: no image", m.Name())
	}
	return request{client: m.client, source: m.Name(), url: fresh.Thumb, limit: maxPhoto}.get(ctx)
}
