package main

import (
	"context"
	"errors"
	"log"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/robdevops/signalbot/cmd/geoguess/geo"
)

// finder finds a photo worth a round in regions ("" is GEO_REGIONS): the
// real search, or a test's.
type finder interface {
	Find(ctx context.Context, regions string) (geo.Photo, error)
}

// The prefetcher's limits: one search's time, and the photos kept beyond
// GEO_BUFFER for rounds in other regions.
const (
	findTimeout = 3 * time.Minute
	extraPhotos = 4
)

// prefetch keeps GEO_BUFFER checked photos ready, so /geo answers at once,
// and looks for one in each region a /geo asked for. It is the only caller
// of Find.
func (s *server) prefetch(ctx context.Context) {
	failures := 0
	for {
		region, ok := s.nextSearch()
		if !ok {
			select {
			case <-s.wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		fctx, cancel := context.WithTimeout(ctx, findTimeout)
		p, err := s.photos.Find(fctx, region)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			failures = 0
			s.add(p, region)
			continue
		}
		s.mu.Lock()
		delete(s.wants, region) // asked again by the next /geo for it
		s.mu.Unlock()
		wait := min(5*time.Second<<failures, 5*time.Minute)
		failures = min(failures+1, 10)
		if errors.Is(err, geo.ErrSlowDown) {
			wait = time.Minute
		}
		s.log.Printf("no photo found (%v); trying again in %s", err, wait)
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
	}
}

// nextSearch is the region to look in next: the first a /geo asked for
// that the buffer lacks, else GEO_REGIONS while the buffer isn't full.
func (s *server) nextSearch() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, region := range slices.Sorted(maps.Keys(s.wants)) {
		if s.match(region) >= 0 {
			delete(s.wants, region)
			continue
		}
		return region, true
	}
	return "", len(s.buffer) < s.cfg.Buffer
}

// add puts a photo found for region in the buffer, and marks it used so no
// later search finds it again.
func (s *server) add(p geo.Photo, region string) {
	s.mu.Lock()
	s.buffer = append(s.buffer, p)
	if over := len(s.buffer) - s.cfg.Buffer - extraPhotos; over > 0 {
		s.buffer = slices.Delete(s.buffer, 0, over)
	}
	s.markUsed(p.Key())
	delete(s.wants, region)
	close(s.changed)
	s.changed = make(chan struct{})
	n := len(s.buffer)
	s.mu.Unlock()
	s.save()
	s.log.Printf("a %s photo is ready; %d in all", p.Source, n)
}

// take is a photo from the buffer for region, waiting up to photoWait for
// the prefetcher to find one.
func (s *server) take(ctx context.Context, region string) (geo.Photo, bool) {
	t := time.NewTimer(s.photoWait)
	defer t.Stop()
	for {
		s.mu.Lock()
		if i := s.match(region); i >= 0 {
			p := s.buffer[i]
			s.buffer = slices.Delete(s.buffer, i, i+1)
			s.mu.Unlock()
			s.wakeUp()
			return p, true
		}
		if region != "" {
			s.wants[region] = true
		}
		changed := s.changed
		s.mu.Unlock()
		s.wakeUp()
		select {
		case <-changed:
		case <-t.C:
			return geo.Photo{}, false
		case <-ctx.Done():
			return geo.Photo{}, false
		}
	}
}

// match is the index of the oldest photo in the buffer that is in region
// ("" is GEO_REGIONS), or -1. s.mu is held.
func (s *server) match(region string) int {
	if region == "" {
		region = s.cfg.Regions
	}
	if region == "" {
		if len(s.buffer) == 0 {
			return -1
		}
		return 0
	}
	seeds, err := s.seedsLocked(region)
	if err != nil {
		return -1
	}
	return slices.IndexFunc(s.buffer, func(p geo.Photo) bool { return seeds.Has(p.Place.CountryCode) })
}

// regionSeeds is the seeds for region, a /geo argument, or an error if it
// names none.
func (s *server) regionSeeds(region string) (*geo.Seeds, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seedsLocked(region)
}

func (s *server) seedsLocked(region string) (*geo.Seeds, error) {
	key := strings.ToUpper(region)
	if seeds := s.seeds[key]; seeds != nil {
		return seeds, nil
	}
	seeds, err := geo.LoadSeeds(region)
	if err == nil && len(s.seeds) < 64 {
		s.seeds[key] = seeds
	}
	return seeds, err
}

// wakeUp tells the prefetcher to look at the buffer again, without waiting.
func (s *server) wakeUp() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// search is the real finder: Mapillary and Panoramax, checked by the geo
// package, with places named by Nominatim.
type search struct {
	sources []geo.Source
	rules   geo.Rules
	places  *geo.Nominatim
	rand    *rand.Rand
	used    func(key string) bool
	log     *log.Logger
	seeds   map[string]*geo.Seeds
	regions string // GEO_REGIONS
}

// Find looks for one photo. It logs only counts of rejections, by reason,
// and errors, which hold no URL.
func (f *search) Find(ctx context.Context, regions string) (geo.Photo, error) {
	if regions == "" {
		regions = f.regions
	}
	seeds := f.seeds[regions]
	if seeds == nil {
		var err error
		if seeds, err = geo.LoadSeeds(regions); err != nil {
			return geo.Photo{}, err
		}
		f.seeds[regions] = seeds
	}
	rejected := map[string]int{}
	finder := geo.Finder{
		Sources: f.sources, Seeds: seeds, Rules: f.rules, Places: f.places, Rand: f.rand, Now: time.Now,
		Used:     f.used,
		Rejected: func(source string, why geo.Reject) { rejected[source+": "+string(why)]++ },
		Failed:   func(err error) { f.log.Printf("search failed: %v", err) },
	}
	p, err := finder.Find(ctx)
	if len(rejected) > 0 {
		f.log.Printf("photos rejected: %v", rejected)
	}
	return p, err
}
