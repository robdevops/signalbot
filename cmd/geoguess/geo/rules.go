package geo

import (
	"math"
	"time"
)

// Reject is why a candidate or its photo was turned down: one of a fixed
// list, so it is safe to log.
type Reject string

func (r Reject) Error() string { return string(r) }

// The reasons a candidate is rejected, from its metadata and then its
// pixels.
const (
	RejectQuality    Reject = "low quality"
	RejectUndated    Reject = "no capture time"
	RejectOld        Reject = "too old"
	RejectSmall      Reject = "too small"
	RejectFisheye    Reject = "fisheye"
	RejectDark       Reject = "not daylight"
	RejectDrift      Reject = "GPS drift"
	RejectSequence   Reject = "same sequence"
	RejectUsed       Reject = "used recently"
	RejectUnreadable Reject = "unreadable image"
	RejectExposure   Reject = "badly exposed"
	RejectContrast   Reject = "low contrast"
	RejectBlurry     Reject = "blurry"
	RejectSky        Reject = "mostly sky"
	RejectDashboard  Reject = "dashboard"
	RejectBare       Reject = "little detail"
	RejectNoPlace    Reject = "no country"
)

// Rules are the metadata checks, which need no download.
type Rules struct {
	MinQuality float64       // the least Mapillary quality_score, when it has one
	MaxAge     time.Duration // the oldest photo taken
}

// The fixed limits of the metadata checks.
const (
	minWidth    = 1024 // pixels
	minGrade    = 3    // Panoramax's C
	minSunAngle = 12   // degrees above the horizon, a margin for camera clocks
	maxDriftKm  = 0.05 // between Mapillary's corrected and raw positions
)

// Check is nil if c passes the metadata checks at now, from cheapest to
// dearest, else the first it fails.
func (r Rules) Check(c Candidate, now time.Time) error {
	switch {
	case c.Quality >= 0 && c.Quality < r.MinQuality:
		return RejectQuality
	case c.Grade > 0 && c.Grade < minGrade:
		return RejectQuality
	case c.Captured.IsZero():
		return RejectUndated
	case now.Sub(c.Captured) > r.MaxAge:
		return RejectOld
	case c.Width > 0 && c.Width < minWidth:
		return RejectSmall
	case c.Fisheye:
		return RejectFisheye
	case SunElevation(c.At, c.Captured) <= minSunAngle:
		return RejectDark
	case Km(c.At, c.Raw) > maxDriftKm:
		return RejectDrift
	}
	return nil
}

// Filter is the candidates that pass the checks, at most one from each
// sequence and none whose Key used reports as recently used, in their order. It
// calls reject with why each other one was turned down.
func (r Rules) Filter(cands []Candidate, now time.Time, used func(id string) bool, reject func(Reject)) []Candidate {
	var out []Candidate
	seqs := map[string]bool{}
	for _, c := range cands {
		err := r.Check(c, now)
		switch {
		case err != nil:
		case used != nil && used(c.Key()):
			err = RejectUsed
		case c.Sequence != "" && seqs[c.Sequence]:
			err = RejectSequence
		}
		if err != nil {
			if reject != nil {
				reject(err.(Reject))
			}
			continue
		}
		seqs[c.Sequence] = true
		out = append(out, c)
	}
	return out
}

// SunElevation is the sun's angle above the horizon in degrees at p and t,
// by NOAA's solar position formulas (good to about a degree for a century
// either side of 2000).
func SunElevation(p Point, t time.Time) float64 {
	t = t.UTC()
	jd := float64(t.UnixNano())/float64(24*time.Hour) + 2440587.5
	T := (jd - 2451545) / 36525 // Julian centuries since J2000

	l0 := math.Mod(280.46646+T*(36000.76983+T*0.0003032), 360) // mean longitude
	m := 357.52911 + T*(35999.05029-0.0001537*T)               // mean anomaly
	e := 0.016708634 - T*(0.000042037+0.0000001267*T)          // orbit eccentricity
	mr := radians(m)
	centre := math.Sin(mr)*(1.914602-T*(0.004817+0.000014*T)) + math.Sin(2*mr)*(0.019993-0.000101*T) + math.Sin(3*mr)*0.000289
	ω := radians(125.04 - 1934.136*T)
	λ := radians(l0 + centre - 0.00569 - 0.00478*math.Sin(ω)) // apparent longitude
	ε0 := 23 + (26+(21.448-T*(46.815+T*(0.00059-T*0.001813)))/60)/60
	ε := radians(ε0 + 0.00256*math.Cos(ω)) // obliquity, corrected
	decl := math.Asin(math.Sin(ε) * math.Sin(λ))

	y := math.Pow(math.Tan(ε/2), 2)
	l0r := radians(l0)
	eqTime := 4 * degrees(y*math.Sin(2*l0r)-2*e*math.Sin(mr)+4*e*y*math.Sin(mr)*math.Cos(2*l0r)-
		0.5*y*y*math.Sin(4*l0r)-1.25*e*e*math.Sin(2*mr)) // minutes

	minutes := float64(t.Hour()*60+t.Minute()) + float64(t.Second())/60 + float64(t.Nanosecond())/6e10
	solarTime := math.Mod(minutes+eqTime+4*p.Lon, 1440)
	if solarTime < 0 {
		solarTime += 1440
	}
	hourAngle := radians(solarTime/4 - 180)
	φ := radians(p.Lat)
	cosZenith := math.Sin(φ)*math.Sin(decl) + math.Cos(φ)*math.Cos(decl)*math.Cos(hourAngle)
	return 90 - degrees(math.Acos(math.Max(-1, math.Min(1, cosZenith))))
}
