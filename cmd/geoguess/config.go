package main

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/robdevops/signalbot/cmd/geoguess/geo"
)

// config is GeoGuess's settings, every one from a GEO_ variable, the only
// ones signalbot passes an addon called geo.
type config struct {
	StateDir   string        // GEO_STATE_DIR: where geo.json and the log are kept
	Token      string        // GEO_MAPILLARY_TOKEN: empty is Panoramax only
	MemLimit   int64         // GEO_MEMLIMIT, in bytes
	Round      time.Duration // GEO_ROUND: a round's default length
	Regions    string        // GEO_REGIONS: continents or country codes; empty is the world
	MinQuality float64       // GEO_MIN_QUALITY: the least Mapillary quality_score
	MaxAge     time.Duration // GEO_MAX_AGE_YEARS, as a duration
	Buffer     int           // GEO_BUFFER: checked photos kept ready
	Keep       []string      // GEO_KEEP: regions always kept with a photo ready, one each
	Wait       time.Duration // GEO_WAIT: how long /geo waits for a photo when none is ready
	Nominatim  string        // GEO_NOMINATIM_URL
	Panoramax  string        // GEO_PANORAMAX_URL
	Mapillary  string        // GEO_MAPILLARY_URL
}

// The shortest and longest round, and the defaults of the settings.
const (
	minRound        = 30 * time.Second
	maxRound        = 30 * time.Minute
	defaultRound    = 3 * time.Minute
	defaultMemLimit = "200MiB"
	minWait         = 5 * time.Second
	maxWait         = 110 * time.Second // under the 2m a direct command may take, with time to reply
	defaultWait     = 50 * time.Second
)

// year is a year as GEO_MAX_AGE_YEARS counts it.
const year = 365*24*time.Hour + 6*time.Hour

// loadConfig reads the settings through getenv (os.Getenv), failing with a
// message that names the setting.
func loadConfig(getenv func(string) string) (config, error) {
	c := config{
		StateDir:  strings.TrimSpace(getenv("GEO_STATE_DIR")),
		Token:     strings.TrimSpace(getenv("GEO_MAPILLARY_TOKEN")),
		Regions:   strings.TrimSpace(getenv("GEO_REGIONS")),
		Nominatim: cmp.Or(strings.TrimSpace(getenv("GEO_NOMINATIM_URL")), geo.NominatimURL),
		Panoramax: cmp.Or(strings.TrimSpace(getenv("GEO_PANORAMAX_URL")), geo.PanoramaxURL),
		Mapillary: cmp.Or(strings.TrimSpace(getenv("GEO_MAPILLARY_URL")), geo.MapillaryURL),
	}
	var errs []error
	if c.StateDir == "" {
		errs = append(errs, errors.New("GEO_STATE_DIR is not set: set it to a directory GeoGuess can write, and create it"))
	}
	var err error
	if c.MemLimit, err = parseSize(cmp.Or(strings.TrimSpace(getenv("GEO_MEMLIMIT")), defaultMemLimit)); err != nil {
		errs = append(errs, fmt.Errorf("GEO_MEMLIMIT: %v", err))
	}
	c.Round = defaultRound
	if v := strings.TrimSpace(getenv("GEO_ROUND")); v != "" {
		if c.Round, err = roundLength(v); err != nil {
			errs = append(errs, fmt.Errorf("GEO_ROUND: %v", err))
		}
	}
	if _, err := geo.LoadSeeds(c.Regions); err != nil {
		errs = append(errs, fmt.Errorf("GEO_REGIONS: %v", err))
	}
	c.MinQuality = 0.5
	if v := strings.TrimSpace(getenv("GEO_MIN_QUALITY")); v != "" {
		if c.MinQuality, err = strconv.ParseFloat(v, 64); err != nil || c.MinQuality < 0 || c.MinQuality > 1 {
			errs = append(errs, fmt.Errorf("GEO_MIN_QUALITY is %q: use a number from 0 to 1", v))
		}
	}
	years := 10.0
	if v := strings.TrimSpace(getenv("GEO_MAX_AGE_YEARS")); v != "" {
		if years, err = strconv.ParseFloat(v, 64); err != nil || years <= 0 || years > 100 {
			errs = append(errs, fmt.Errorf("GEO_MAX_AGE_YEARS is %q: use a number of years above 0", v))
		}
	}
	c.MaxAge = time.Duration(years * float64(year))
	c.Buffer = 3
	if v := strings.TrimSpace(getenv("GEO_BUFFER")); v != "" {
		if c.Buffer, err = strconv.Atoi(v); err != nil || c.Buffer < 1 || c.Buffer > 20 {
			errs = append(errs, fmt.Errorf("GEO_BUFFER is %q: use a whole number from 1 to 20", v))
		}
	}
	for _, region := range strings.Split(getenv("GEO_KEEP"), ",") {
		region = strings.TrimSpace(region)
		if region == "" {
			continue
		}
		if _, err := geo.LoadSeeds(region); err != nil {
			errs = append(errs, fmt.Errorf("GEO_KEEP: %v", err))
			continue
		}
		c.Keep = append(c.Keep, region)
	}
	c.Wait = defaultWait
	if v := strings.TrimSpace(getenv("GEO_WAIT")); v != "" {
		if c.Wait, err = time.ParseDuration(v); err != nil || c.Wait < minWait || c.Wait > maxWait {
			errs = append(errs, fmt.Errorf("GEO_WAIT is %q: use 5s to 110s, such as 50s", v))
		}
	}
	// Over TLS only: the geocoder is sent each round's answer.
	for _, u := range []struct{ name, url string }{
		{"GEO_NOMINATIM_URL", c.Nominatim}, {"GEO_PANORAMAX_URL", c.Panoramax}, {"GEO_MAPILLARY_URL", c.Mapillary},
	} {
		if !strings.HasPrefix(u.url, "https://") {
			errs = append(errs, fmt.Errorf("%s must start with https://", u.name))
		}
	}
	return c, errors.Join(errs...)
}

// roundLength reads a round's length, such as 90s or 5m, from 30s to 30m.
func roundLength(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d < minRound || d > maxRound {
		return 0, fmt.Errorf("%q isn't a round length: use 30s to 30m, such as 90s or 5m", s)
	}
	return d, nil
}

// sizeUnits are the suffixes parseSize takes, as GOMEMLIMIT does.
var sizeUnits = []struct {
	suffix string
	mult   int64
}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}}

// parseSize reads a number of bytes written as GOMEMLIMIT is, such as
// 200MiB or 1GiB.
func parseSize(s string) (int64, error) {
	mult := int64(1)
	num := s
	for _, u := range sizeUnits {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			num, mult = n, u.mult
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil || n < 0 || n*float64(mult) >= math.MaxInt64 {
		return 0, fmt.Errorf("%q isn't a size: use a number of bytes with B, KiB, MiB or GiB, such as 200MiB", s)
	}
	if n*float64(mult) < 16<<20 {
		return 0, fmt.Errorf("%q is too small: GeoGuess needs at least 16MiB", s)
	}
	return int64(n * float64(mult)), nil
}

// checkDir fails unless dir can be listed and written, leaving nothing.
func checkDir(dir string) error {
	if _, err := os.ReadDir(dir); err != nil {
		return fmt.Errorf("GEO_STATE_DIR %s can't be read (%v): create it, writable by the bot's user", dir, err)
	}
	f, err := os.CreateTemp(dir, ".check-*")
	if err == nil {
		f.Close()
		err = os.Remove(f.Name())
	}
	if err != nil {
		return fmt.Errorf("GEO_STATE_DIR %s can't be written (%v): make it writable by the bot's user", dir, err)
	}
	return nil
}
