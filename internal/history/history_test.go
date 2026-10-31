package history

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/chat"
)

func turns(n int) func() int { return func() int { return n } }

func TestChatsAreCapped(t *testing.T) {
	s := New(turns(2))
	var last chat.ChatID
	for i := 0; i < maxChats+50; i++ {
		last = chat.ChatID(fmt.Sprintf("g:%d", i))
		s.Add(last, Entry{Role: "user", Content: "x"})
	}
	if s.Len() > maxChats {
		t.Fatalf("history grew past maxChats: %d", s.Len())
	}
	if len(s.Get(last)) == 0 {
		t.Fatal("the chat just added was dropped")
	}
}

// Disappearing messages: the entries are erased from memory once their
// time runs out, and the chat is forgotten once nothing is left.
func TestExpiredEntriesAreErased(t *testing.T) {
	s := New(turns(2))
	var forgotten atomic.Value
	s.OnForget(func(c chat.ChatID) { forgotten.Store(c) })
	soon := time.Now().Add(50 * time.Millisecond)
	s.Add("k", Entry{Role: "user", Content: "a", Expires: soon}, Entry{Role: "assistant", Content: "b", Expires: soon})
	s.Add("k", Entry{Role: "user", Content: "kept"}, Entry{Role: "assistant", Content: "kept"})
	if n := len(s.Get("k")); n != 4 {
		t.Fatalf("history before expiry has %d entries, want 4", n)
	}
	time.Sleep(150 * time.Millisecond)
	s.mu.Lock()
	n := len(s.chats["k"])
	s.mu.Unlock()
	if n != 2 {
		t.Fatalf("expired entries still in memory: %d left, want 2", n)
	}
	if forgotten.Load() != nil {
		t.Fatal("a chat with entries left was forgotten")
	}
	s.Add("j", Entry{Role: "user", Content: "x", Expires: time.Now().Add(20 * time.Millisecond)})
	time.Sleep(100 * time.Millisecond)
	if forgotten.Load() != chat.ChatID("j") {
		t.Fatalf("OnForget got %v, want j", forgotten.Load())
	}
}

// No message is remembered for more than a day, whatever the chat's
// disappearing-messages setting; a shorter timer wins.
func TestExpiry(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct{ timer, want time.Duration }{
		{0, 24 * time.Hour},
		{time.Hour, time.Hour},
		{7 * 24 * time.Hour, 24 * time.Hour},
	} {
		if got := Expiry(now, tc.timer); !got.Equal(now.Add(tc.want)) {
			t.Errorf("timer %v: expires in %v, want %v", tc.timer, got.Sub(now), tc.want)
		}
	}
}

// However many entries a chat holds, it has at most one expiry timer, and
// it is set for the earliest entry to expire.
func TestOneEraserPerChat(t *testing.T) {
	s := New(turns(100))
	now := time.Now()
	for i := 0; i < 50; i++ {
		s.Add("g:x", Entry{Role: "user", Content: "m", Expires: now.Add(time.Hour + time.Duration(i)*time.Minute)})
	}
	s.Add("g:x", Entry{Role: "user", Content: "soon", Expires: now.Add(30 * time.Millisecond)})
	s.mu.Lock()
	n, at := len(s.erasers), s.erasers["g:x"].at
	s.mu.Unlock()
	if n != 1 || !at.Equal(now.Add(30*time.Millisecond)) {
		t.Fatalf("%d timers, next at %v", n, at.Sub(now))
	}
	time.Sleep(150 * time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.chats["g:x"]) != 50 || !s.erasers["g:x"].at.Equal(now.Add(time.Hour)) {
		t.Fatalf("after the first expiry: %d entries, next at %v", len(s.chats["g:x"]), s.erasers["g:x"].at.Sub(now))
	}
}

// Only HISTORY_TURNS turns are kept.
func TestTurnsAreCapped(t *testing.T) {
	s := New(turns(2))
	for i := 0; i < 5; i++ {
		s.Add("c", Entry{Role: "user", Content: fmt.Sprint(i)}, Entry{Role: "assistant", Content: "ok"})
	}
	h := s.Get("c")
	if len(h) != 4 || h[0].Content != "3" {
		t.Fatalf("kept %v", h)
	}
}
