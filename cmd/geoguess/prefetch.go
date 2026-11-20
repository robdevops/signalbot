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

// retry is when a GEO_KEEP region whose search failed is searched again,
// and how many times in a row it has failed.
type retry struct {
	at       time.Time
	failures int
}

// prefetch keeps GEO_BUFFER checked photos ready, so /geo answers at once,
// and looks for one in each region a /geo asked for. It is the only caller
// of Find. A GEO_KEEP region whose search fails waits its own backoff,
// while the rest go on, so one hard region can't starve the buffer.
func (s *server) prefetch(ctx context.Context) {
	failures := 0
	for {
		region, ok, next := s.nextSearch(time.Now())
		if !ok {
			if !s.idle(ctx, next) {
				return
			}
			continue
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
		keep := slices.Contains(s.cfg.Keep, region)
		var wait time.Duration
		if keep {
			r := s.retries[region]
			wait = backoff(r.failures, err)
			s.retries[region] = retry{time.Now().Add(wait), min(r.failures+1, 10)}
		}
		s.mu.Unlock()
		if keep && !errors.Is(err, geo.ErrSlowDown) {
			s.log.Printf("no photo found for a kept region (%v); trying it again in %s", err, wait)
			continue
		}
		wait = backoff(failures, err)
		failures = min(failures+1, 10)
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

// backoff is how long to wait after failures failed searches in a row and
// then err: from 5s, doubling to 5 minutes, or a minute when asked to slow
// down.
func backoff(failures int, err error) time.Duration {
	if errors.Is(err, geo.ErrSlowDown) {
		return time.Minute
	}
	return min(5*time.Second<<failures, 5*time.Minute)
}

// idle waits for work: a wake, or next, when a GEO_KEEP region may be
// searched again (zero for none). It is false once ctx is done.
func (s *server) idle(ctx context.Context, next time.Time) bool {
	var due <-chan time.Time
	if !next.IsZero() {
		t := time.NewTimer(time.Until(next))
		defer t.Stop()
		due = t.C
	}
	select {
	case <-s.wake:
	case <-due:
	case <-ctx.Done():
		return false
	}
	return true
}

// nextSearch is the region to look in next: the first a /geo asked for
// that the buffer lacks, else the first GEO_KEEP region it lacks whose
// retry is due at now, else GEO_REGIONS while the buffer isn't full. With
// none, next is the earliest retry still to come, or zero.
func (s *server) nextSearch(now time.Time) (region string, ok bool, next time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, region := range slices.Sorted(maps.Keys(s.wants)) {
		if s.match(region) >= 0 {
			delete(s.wants, region)
			continue
		}
		return region, true, time.Time{}
	}
	for _, region := range s.cfg.Keep {
		if s.match(region) >= 0 {
			delete(s.retries, region)
			continue
		}
		r, failed := s.retries[region]
		if !failed || !now.Before(r.at) {
			return region, true, time.Time{}
		}
		if next.IsZero() || r.at.Before(next) {
			next = r.at
		}
	}
	return "", len(s.buffer) < s.cfg.Buffer, next
}

// add puts a photo found for region in the buffer, and marks it used so no
// later search finds it again. Past GEO_BUFFER and extraPhotos it drops the
// oldest, but never a GEO_KEEP region's last.
func (s *server) add(p geo.Photo, region string) {
	s.mu.Lock()
	s.buffer = append(s.buffer, p)
	for over := len(s.buffer) - s.cfg.Buffer - extraPhotos; over > 0; over-- {
		i := slices.IndexFunc(s.buffer, s.droppable)
		if i < 0 {
			break
		}
		s.buffer = slices.Delete(s.buffer, i, i+1)
	}
	s.markUsed(p.Key())
	delete(s.wants, region)
	delete(s.retries, region)
	close(s.changed)
	s.changed = make(chan struct{})
	n := len(s.buffer)
	s.mu.Unlock()
	s.save()
	s.log.Printf("a %s photo is ready; %d in all", p.Source, n)
}

// droppable reports whether p, in the buffer, can leave it without leaving
// a GEO_KEEP region with no photo. s.mu is held.
func (s *server) droppable(p geo.Photo) bool {
	for _, region := range s.cfg.Keep {
		if !s.in(p, region) {
			continue
		}
		n := 0
		for _, q := range s.buffer {
			if s.in(q, region) {
				n++
			}
		}
		if n < 2 {
			return false
		}
	}
	return true
}

// take is a photo from the buffer for region, waiting up to wait for the
// prefetcher to find one. Taking one wakes the prefetcher, which refills a
// GEO_KEEP region it used up.
func (s *server) take(ctx context.Context, region string, wait time.Duration) (geo.Photo, bool) {
	t := time.NewTimer(max(wait, 0))
	defer t.Stop()
	for {
		s.mu.Lock()
		if i := s.pick(region); i >= 0 {
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

// pick is the index of the photo take gives for region, or -1: the oldest
// in it, but for a region not in GEO_KEEP, one a GEO_KEEP region can spare
// if there is one, so a plain /geo leaves the kept photo for its region.
// s.mu is held.
func (s *server) pick(region string) int {
	if !slices.ContainsFunc(s.cfg.Keep, func(k string) bool { return strings.EqualFold(k, region) }) {
		if i := slices.IndexFunc(s.buffer, func(p geo.Photo) bool { return s.in(p, region) && s.droppable(p) }); i >= 0 {
			return i
		}
	}
	return s.match(region)
}

// match is the index of the oldest photo in the buffer that is in region
// ("" is GEO_REGIONS), or -1. s.mu is held.
func (s *server) match(region string) int {
	return slices.IndexFunc(s.buffer, func(p geo.Photo) bool { return s.in(p, region) })
}

// in reports whether p is in region ("" is GEO_REGIONS, and empty too the
// world). s.mu is held.
func (s *server) in(p geo.Photo, region string) bool {
	if region == "" {
		region = s.cfg.Regions
	}
	if region == "" {
		return true
	}
	seeds, err := s.seedsLocked(region)
	return err == nil && seeds.Has(p.Place.CountryCode)
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
