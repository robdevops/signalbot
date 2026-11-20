package geo

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PanoramaxURL is Panoramax's meta-catalogue, which lists the photos of
// every instance.
const PanoramaxURL = "https://api.panoramax.xyz"

// panoramaxLimit is the most items one search returns.
const panoramaxLimit = 100

// maxPanoramaxAnswer caps one search's answer; items carry their EXIF.
const maxPanoramaxAnswer = 16 << 20

// Panoramax searches Panoramax's STAC API. It needs no key; it publishes no
// rate limit, so it is called at most once a second.
type Panoramax struct {
	url    string
	client *http.Client
	pace   *pacer
}

// NewPanoramax is a Panoramax client for the catalogue at base
// (PanoramaxURL).
func NewPanoramax(base string, client *http.Client) *Panoramax {
	return &Panoramax{url: base, client: client, pace: &pacer{every: time.Second}}
}

// Name is "Panoramax".
func (p *Panoramax) Name() string { return "Panoramax" }

type panoramaxItem struct {
	ID         string        `json:"id"`
	Geometry   *geoJSONPoint `json:"geometry"`
	Collection string        `json:"collection"`
	Providers  []struct {
		Name  string   `json:"name"`
		Roles []string `json:"roles"`
	} `json:"providers"`
	Assets map[string]struct {
		Href string `json:"href"`
	} `json:"assets"`
	Properties struct {
		Datetime    string `json:"datetime"` // UTC
		License     string `json:"license"`  // an SPDX ID
		Orientation struct {
			FieldOfView *float64 `json:"field_of_view"`
			Dimensions  []int    `json:"sensor_array_dimensions"`
		} `json:"pers:interior_orientation"`
		Accuracy *float64 `json:"quality:horizontal_accuracy"` // m
		Density  *float64 `json:"panoramax:horizontal_pixel_density"`
	} `json:"properties"`
}

// Candidates searches b.
func (p *Panoramax) Candidates(ctx context.Context, b BBox) ([]Candidate, error) {
	if err := p.pace.wait(ctx); err != nil {
		return nil, err
	}
	q := url.Values{"bbox": {b.String()}, "limit": {strconv.Itoa(panoramaxLimit)}}
	var a struct {
		Features []panoramaxItem `json:"features"`
	}
	r := request{client: p.client, source: p.Name(), url: p.url + "/api/search?" + q.Encode(), limit: maxPanoramaxAnswer}
	if err := r.getJSON(ctx, &a); err != nil {
		if se, ok := err.(StatusError); ok && se.Code == http.StatusTooManyRequests {
			return nil, ErrSlowDown
		}
		return nil, err
	}
	out := make([]Candidate, 0, len(a.Features))
	for _, it := range a.Features {
		if c, ok := p.candidate(it); ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// candidate is it as a Candidate, if it has an ID, a position, a licence
// the credit can name, and an SD picture.
func (p *Panoramax) candidate(it panoramaxItem) (Candidate, bool) {
	at, ok := it.Geometry.point()
	licence := licenceName(it.Properties.License)
	sd := it.Assets["sd"].Href
	if it.ID == "" || !ok || licence == "" || sd == "" {
		return Candidate{}, false
	}
	props := it.Properties
	c := Candidate{
		ID:       it.ID,
		Source:   "Panoramax",
		Creator:  producer(it),
		Licence:  licence,
		Link:     p.url + "/#focus=pic&pic=" + url.QueryEscape(it.ID),
		At:       at,
		Raw:      at,
		Pano:     props.Orientation.FieldOfView != nil && *props.Orientation.FieldOfView == 360,
		Quality:  -1,
		Sequence: it.Collection,
		image:    sd,
	}
	if d := props.Orientation.Dimensions; len(d) == 2 {
		c.Width, c.Height = d[0], d[1]
	}
	if t, err := time.Parse(time.RFC3339, props.Datetime); err == nil {
		c.Captured = t.UTC()
	}
	c.Grade = grade(c.Pano, props.Density, props.Accuracy)
	return c, true
}

// producer is the first provider with the producer role: the author.
func producer(it panoramaxItem) string {
	for _, pr := range it.Providers {
		for _, role := range pr.Roles {
			if role == "producer" {
				return pr.Name
			}
		}
	}
	return ""
}

// licenceName is an SPDX ID as the credit shows it: "CC-BY-SA-4.0" is
// "CC BY-SA 4.0", "etalab-2.0" is "Etalab 2.0". It is empty for a licence
// that isn't open (only Creative Commons and Etalab are).
func licenceName(spdx string) string {
	parts := strings.Split(spdx, "-")
	if len(parts) < 2 {
		return ""
	}
	last := len(parts) - 1
	switch first := strings.ToLower(parts[0]); {
	case first == "cc" && last >= 2:
		return "CC " + strings.Join(parts[1:last], "-") + " " + parts[last]
	case first == "cc0":
		return "CC0 " + parts[last]
	case first == "etalab" && last == 1:
		return "Etalab " + parts[last]
	}
	return ""
}

// grade is the A–E grade (5 to 1) that Panoramax's viewer shows, from the
// picture's horizontal pixel density and GPS accuracy:
// round(0.8 × resolution + 0.2 × GPS). Each is often missing, which is
// unknown rather than bad: the grade is then the known one's alone, or 0 if
// neither is known.
func grade(pano bool, density, accuracy *float64) int {
	res := 0
	if density != nil {
		d := *density
		switch {
		case pano && d < 15:
			res = 3
		case pano && d < 30:
			res = 4
		case pano:
			res = 5
		case d < 10:
			res = 1
		case d < 15:
			res = 2
		case d < 30:
			res = 3
		default:
			res = 4
		}
	}
	gps := 0
	if accuracy != nil {
		switch a := *accuracy; {
		case a < 0.5:
			gps = 5
		case a <= 2:
			gps = 4
		case a <= 5:
			gps = 3
		case a <= 10:
			gps = 2
		default:
			gps = 1
		}
	}
	switch {
	case res == 0:
		return gps
	case gps == 0:
		return res
	}
	return int(math.Round(0.8*float64(res) + 0.2*float64(gps)))
}

// Fetch downloads c's SD picture, 2048 px wide, from the instance that holds
// it.
func (p *Panoramax) Fetch(ctx context.Context, c Candidate) ([]byte, error) {
	if c.image == "" {
		return nil, fmt.Errorf("%s: no image", p.Name())
	}
	if err := p.pace.wait(ctx); err != nil {
		return nil, err
	}
	return request{client: p.client, source: p.Name(), url: c.image, limit: maxPhoto}.get(ctx)
}
