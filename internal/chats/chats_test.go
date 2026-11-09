package chats

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// slots is a worker pool of n slots, like the bot's.
type slots struct {
	sem chan struct{}
	wg  sync.WaitGroup
}

func newSlots(n int) *slots { return &slots{sem: make(chan struct{}, n)} }

func (p *slots) spawn(worker func()) bool {
	select {
	case p.sem <- struct{}{}:
	default:
		return false
	}
	p.wg.Go(func() {
		defer func() { <-p.sem }()
		worker()
	})
	return true
}

// blocking is a store on pool whose workers wait until release is closed.
func blocking(release chan struct{}, pool *slots) *Store[string] {
	return New(10, func([]string) { <-release }, pool.spawn)
}

// One chat's messages are handled in order by one worker, which gets
// everything waiting at once; at most maxQueue wait.
func TestMailboxInOrder(t *testing.T) {
	release := make(chan struct{})
	var got []string
	pool := newSlots(8)
	s := New(10, func(rs []string) {
		if rs[0] == "0" {
			<-release
		}
		got = append(got, fmt.Sprint(rs))
	}, pool.spawn)
	for i := range maxQueue + 2 {
		err := s.Post("g:1", fmt.Sprint(i))
		if want := i <= maxQueue; (err == nil) != want {
			t.Errorf("message %d: err %v", i, err)
		}
	}
	if n := len(pool.sem); n != 1 {
		t.Errorf("%d workers for one chat", n)
	}
	close(release)
	pool.wg.Wait()
	if fmt.Sprint(got) != "[[0] [1 2 3 4]]" {
		t.Errorf("handled %v", got)
	}
	if len(s.chats) != 0 {
		t.Error("an idle chat with nothing kept was not deleted")
	}
}

// Different chats run in parallel, each on its own slot, and a full pool
// drops new work.
func TestChatsInParallel(t *testing.T) {
	release := make(chan struct{})
	pool := newSlots(2)
	s := blocking(release, pool)
	if s.Post("g:1", "") != nil || s.Post("g:2", "") != nil {
		t.Fatal("two chats did not get two slots")
	}
	if err := s.Post("g:3", ""); err != ErrNoSlot {
		t.Errorf("full pool: err %v", err)
	}
	if err := s.Post("g:1", ""); err != nil {
		t.Errorf("a busy chat refused a message: %v", err)
	}
	close(release)
	pool.wg.Wait()
}

// A busy chat is never evicted, so it never gets a second worker.
func TestBusyChatNotEvicted(t *testing.T) {
	release := make(chan struct{})
	pool := newSlots(4)
	s := blocking(release, pool)
	s.max = 2
	s.Post("g:busy", "")
	s.Add("g:idle", time.Now(), 0, Entry{Role: "user", Content: "hi"})
	s.Add("g:new", time.Now(), 0, Entry{Role: "user", Content: "hi"})
	if s.chats["g:busy"] == nil || s.chats["g:new"] == nil || s.chats["g:idle"] != nil {
		t.Errorf("kept %v", s.chats)
	}
	s.Post("g:busy2", "") // evicts g:new
	if err := s.Post("g:third", ""); err != errFull {
		t.Errorf("all chats busy: err %v", err)
	}
	close(release)
	pool.wg.Wait()
}

// Over its cap, history drops to half of it, not by one, so its start stays
// the same for several messages; under 3 it slides by one.
func TestHistoryCapped(t *testing.T) {
	s := New[string](4, nil, nil) // messages, not exchanges
	var lens []int
	for i := range 5 {
		s.Add("u:a", time.Now(), 0, Entry{Role: "user", Content: fmt.Sprint(i)}, Entry{Role: "assistant", Content: "ok"})
		lens = append(lens, len(s.History("u:a")))
	}
	h := s.History("u:a")
	if !slices.Equal(lens, []int{2, 4, 2, 4, 2}) || h[0].Content != "4" {
		t.Errorf("lengths %v, history %v", lens, h)
	}
	for n, want := range map[int]int{1: 1, 2: 2, 3: 2, 4: 2, 5: 3, 10: 5, 11: 6} {
		if got := keep(n); got != want {
			t.Errorf("keep(%d) = %d, want %d", n, got, want)
		}
	}
	if exp := time.Until(h[0].Expires); exp < maxAge-time.Minute || exp > maxAge {
		t.Errorf("expires in %v", exp)
	}
}

// History goes when the chat's disappearing-messages timer runs out, and the
// chat's names and follow-up counts go with it.
func TestHistoryExpires(t *testing.T) {
	s := New[string](10, nil, nil)
	s.Learn("g:1", "Alice", "id-a")
	s.SetFollowUp("g:1", 0, 2)
	s.Add("g:1", time.Now(), 30*time.Millisecond, Entry{Role: "user", Content: "hi"})
	if len(s.History("g:1")) != 1 || s.FollowUp("g:1", 0) != 2 {
		t.Fatal("not kept")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.chats)
		s.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the chat was not forgotten")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(s.Names("g:1")) != 0 || s.FollowUp("g:1", 0) != 0 {
		t.Error("names or counts outlived the history")
	}
}

// Expiry counts from when the message arrived, not from when it was added.
func TestExpiryFromStart(t *testing.T) {
	s := New[string](10, nil, nil)
	s.Add("u:a", time.Now().Add(-time.Hour), 2*time.Hour, Entry{Role: "user", Content: "hi"})
	if exp := time.Until(s.History("u:a")[0].Expires); exp > time.Hour {
		t.Errorf("expires in %v", exp)
	}
	s.Add("u:b", time.Now().Add(-time.Minute), 30*time.Second, Entry{Role: "user", Content: "hi"})
	if len(s.History("u:b")) != 0 {
		t.Error("an entry already expired was kept")
	}
}

// A chat that got no history keeps nothing once its worker ends, and goes
// once its message keys lapse.
func TestNoHistoryKeepsNothing(t *testing.T) {
	var s *Store[string]
	pool := newSlots(1)
	s = New(10, func([]string) {
		s.Learn("g:1", "Alice", "id-a")
		s.SetFollowUp("g:1", 0, 2)
		s.Repeat("g:1", "k")
	}, pool.spawn)
	s.Post("g:1", "")
	pool.wg.Wait()
	if len(s.Names("g:1")) != 0 || s.FollowUp("g:1", 0) != 0 {
		t.Error("names or counts kept with no history")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chats["g:1"]
	if c == nil || c.timer == nil {
		t.Fatal("the chat's message key was not kept until it lapses")
	}
	c.recent["k"] = time.Now().Add(-window)
	s.tidy("g:1", c)
	if len(s.chats) != 0 {
		t.Error("the chat was kept after its message key lapsed")
	}
}

// When history expires during a reply, the chat stays until its worker ends.
func TestExpiryWhileBusy(t *testing.T) {
	release := make(chan struct{})
	pool := newSlots(1)
	s := blocking(release, pool)
	s.Add("g:1", time.Now(), 10*time.Millisecond, Entry{Role: "user", Content: "hi"})
	s.Post("g:1", "")
	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	c := s.chats["g:1"]
	s.mu.Unlock()
	if c == nil {
		t.Fatal("a busy chat was deleted")
	}
	if len(s.History("g:1")) != 0 {
		t.Error("expired history kept")
	}
	close(release)
	pool.wg.Wait()
}

func TestRepeat(t *testing.T) {
	s := New[string](10, nil, nil)
	if s.Repeat("g:1", "k") || !s.Repeat("g:1", "k") || s.Repeat("g:2", "k") || s.Repeat("g:1", "other") {
		t.Error("wrong repeats")
	}
}

func TestClaim(t *testing.T) {
	names := map[string]string{}
	Claim(names, "Alice", "id-a")
	Claim(names, "Bob", "")
	Claim(names, "bob", "id-b")
	Claim(names, "", "id-c")
	if len(names) != 2 || names["alice"] != "id-a" || names["bob"] != "" {
		t.Errorf("names %v", names)
	}
}

func TestNames(t *testing.T) {
	s := New[string](10, nil, nil)
	s.Learn("g:1", "Alice", "id-a")
	s.Learn("g:1", "alice", "id-a")
	s.Learn("g:1", "Bob", "id-b")
	s.Learn("g:1", "BOB", "id-imposter")
	got := s.Names("g:1")
	if len(got) != 2 || got["alice"] != "id-a" || got["bob"] != "" {
		t.Errorf("names %v", got)
	}
	s.ForgetNames("g:1")
	if len(s.Names("g:1")) != 0 {
		t.Error("names not forgotten")
	}
}

func TestFollowUps(t *testing.T) {
	s := New[string](10, nil, nil)
	s.SetFollowUp("g:1", 1, 2)
	s.SetFollowUp("g:1", 1, 1)
	if s.FollowUp("g:1", 1) != 1 || s.FollowUp("g:1", 0) != 0 {
		t.Error("wrong count")
	}
	s.SetFollowUp("g:1", 1, 0)
	if s.FollowUp("g:1", 1) != 0 {
		t.Error("count not cleared")
	}
}

// Forget drops only the entries holding the message; a merged one goes
// whole, and the bot's replies stay.
func TestForget(t *testing.T) {
	s := New(10, func([]string) {}, newSlots(1).spawn)
	now := time.Now()
	s.Add("k", now, 0,
		Entry{Role: "user", Content: "a", Refs: []string{"alice\x001"}},
		Entry{Role: "assistant", Content: "reply"},
		Entry{Role: "user", Content: "b and c", Refs: []string{"bob\x002", "alice\x003"}},
		Entry{Role: "user", Content: "d", Refs: []string{"bob\x004"}})
	s.Forget("k", "alice\x003")
	s.Forget("k", "")
	s.Forget("other", "bob\x004")
	var got []string
	for _, e := range s.History("k") {
		got = append(got, e.Content)
	}
	if !slices.Equal(got, []string{"a", "reply", "d"}) {
		t.Fatalf("history %q", got)
	}
}
