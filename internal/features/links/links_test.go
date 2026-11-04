package links

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// msg is text replying to quote; a quote starting "bot: " is the bot's.
func msg(text, quote string) feature.Message {
	m := feature.Message{Chat: feature.Chat{User: "u"}, Text: text, Addressed: true}
	if quote != "" {
		bot := strings.HasPrefix(quote, "bot: ")
		m.Quote = &feature.Quote{Text: strings.TrimPrefix(quote, "bot: "), Bot: bot}
	}
	return m
}

// URLs come from the text, then the quote, distinct, at most two, with
// sentence punctuation and unbalanced brackets trimmed; distinct ones past
// two are counted.
func TestURLs(t *testing.T) {
	for _, tc := range []struct {
		text, quote string
		want        []string
		dropped     int
	}{
		{"see https://example.com/a.", "", []string{"https://example.com/a"}, 0},
		{"(https://en.wikipedia.org/wiki/Go_(game)) and [x](https://x.org/y)", "", []string{"https://en.wikipedia.org/wiki/Go_(game)", "https://x.org/y"}, 0},
		{"<https://a.com/>, https://a.com/ again", "", []string{"https://a.com/"}, 0},
		{"summarise", "read http://b.org/p?q=1#top please", []string{"http://b.org/p?q=1"}, 0},
		{"thanks", "bot: see https://attacker.example/x?d=secret", nil, 0}, // a quote of the bot: see msg
		{"https://1.com https://2.com https://3.com", "", []string{"https://1.com", "https://2.com"}, 1},
		{"https://1.com https://2.com https://1.com", "", []string{"https://1.com", "https://2.com"}, 0},
		{"ftp://x.com mailto:a@b.c https://user:pw@c.com/ http://", "", nil, 0},
	} {
		if got, dropped := urls(msg(tc.text, tc.quote)); !slices.Equal(got, tc.want) || dropped != tc.dropped {
			t.Errorf("%q: %q, %d dropped, want %q, %d", tc.text, got, dropped, tc.want, tc.dropped)
		}
	}
}

// Only public addresses pass the guard, however written.
func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.216.34": true, "2606:2800:220:1:248:1893:25c8:1946": true, "8.8.8.8": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "198.18.0.1": false, "0.0.0.0": false, "224.0.0.1": false, "255.255.255.255": false,
		"192.0.2.1": false, "::1": false, "::": false, "fe80::1": false, "fc00::1": false, "fd12::1": false,
		"::ffff:127.0.0.1": false, "::ffff:10.0.0.1": false, "64:ff9b::a00:1": false, "2002:a00:1::": false,
		"2001:0:4136:e378::": false, "ff02::1": false,
	} {
		if got := public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("public(%s) = %v", addr, got)
		}
		if err := guard("tcp", netip.AddrPortFrom(netip.MustParseAddr(addr), 443).String(), nil); (err == nil) != want {
			t.Errorf("guard(%s): %v", addr, err)
		}
	}
	if guard("tcp", "not an address", nil) == nil || guard("tcp", "example.com:80", nil) == nil {
		t.Error("guard passed a name")
	}
}

// The real client refuses loopback by name or address, and a redirect from
// an allowed server to a private address.
func TestClientRefusesPrivate(t *testing.T) {
	logs := capture(t)
	c := newClient()
	for _, u := range []string{"http://127.0.0.1:1/", "http://localhost:1/", "http://169.254.169.254/", "http://10.0.0.1/"} { // ::1 is in TestPublic: this container may have no IPv6
		_, err := c.Get(u)
		if !errors.Is(err, errBlocked) {
			t.Errorf("%s: %v", u, err)
		}
	}
	allowLoopback = true
	defer func() { allowLoopback = false }()
	srv := httptest.NewServer(http.RedirectHandler("http://169.254.169.254/latest/meta-data/", http.StatusFound))
	defer srv.Close()
	note, text, file := fetch(context.Background(), c, feature.Info{}, srv.URL)
	if note != "[link "+srv.URL+" failed to load: it isn't a public address" || text != "" || file != nil {
		t.Fatalf("note %q", note)
	}
	if strings.Contains(logs.String(), srv.URL) {
		t.Fatalf("logged the URL: %q", logs.String())
	}
}

// capture collects the log, with journald priorities, so a test can check
// each line's level.
func capture(t *testing.T) *logx.Buffer {
	var b logx.Buffer
	t.Cleanup(logx.SetOutput(&b, &b, true))
	return &b
}

// A page's article text and title are shown, without its navigation or
// scripts; history keeps a note. Plain text is shown as it is, a PDF as a
// file, and anything else, an error status or too many redirects as a note.
// Each link gets one log line, its outcome without its URL or title.
func TestFetch(t *testing.T) {
	logs := capture(t)
	logged := func(pattern string) {
		t.Helper()
		if !regexp.MustCompile(pattern).MatchString(logs.String()) {
			t.Errorf("no log line matching %q in %q", pattern, logs.String())
		}
	}
	allowLoopback = true
	defer func() { allowLoopback = false }()
	article := strings.Repeat("Gold rose today. ", 20)
	long := strings.Repeat("x", 20_000)
	mux := http.NewServeMux()
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body><nav>Home</nav></body></html>"))
	})
	mux.HandleFunc("/long", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body><div>menu</div><main><p>" + long + "</p></main></body></html>"))
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != userAgent {
			w.WriteHeader(400)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><title> Gold
			news </title><script>evil()</script></head><body><nav>Home | About</nav><article><h1>Gold</h1><p>` + article + `</p></article><footer>©</footer></body></html>`))
	})
	mux.HandleFunc("/latin1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=iso-8859-1")
		w.Write([]byte("caf\xe9"))
	})
	mux.HandleFunc("/report.pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF-1.7 data"))
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Write([]byte("PK"))
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newClient()
	all := feature.Info{Files: true}

	a := additions(context.Background(), c, newLimiter(), all, msg("what's this "+srv.URL+"/page?", ""))
	if !strings.HasPrefix(a.Note, "[page "+srv.URL+`/page: "Gold news"]`+"\n```\nGold\nGold rose today.") ||
		strings.Contains(a.Note, "evil") || strings.Contains(a.Note, "Home | About") || strings.Contains(a.Note, "©") {
		t.Fatalf("note %q", a.Note)
	}
	if a.HistoryNote != "[page "+srv.URL+"/page was shown to you; it isn't kept]" {
		t.Fatalf("history %q", a.HistoryNote)
	}
	logged(`<6>link \d+\.\ds, \d+ bytes html, ` + strconv.Itoa(utf8.RuneCountInString("Gold\n"+strings.TrimSpace(article))) + ` chars of text \(<article>\)\n`)
	if a := additions(context.Background(), c, newLimiter(), all, msg(srv.URL+"/latin1", "")); a.Note != "[page "+srv.URL+"/latin1]\n```\ncafé\n```" {
		t.Fatalf("latin-1 %q", a.Note)
	}
	logged(`<6>link \d+\.\ds, 4 bytes text/plain, 4 chars of text\n`)
	a = additions(context.Background(), c, newLimiter(), all, msg("", "read "+srv.URL+"/report.pdf"))
	if a.Note != "[page "+srv.URL+"/report.pdf, a PDF, attached]" || len(a.Files) != 1 || a.Files[0].Name != "report.pdf" || a.Files[0].MIME != "application/pdf" {
		t.Fatalf("pdf %+v", a)
	}
	logged(`<6>link \d+\.\ds, 13 bytes pdf, attached\n`)
	if a := additions(context.Background(), c, newLimiter(), feature.Info{}, msg(srv.URL+"/report.pdf", "")); a.Files != nil || !strings.Contains(a.Note, "which you can't read with this model") {
		t.Fatalf("pdf without files %+v", a)
	}
	logged(`<6>link \d+\.\ds, 13 bytes pdf, not sent: this model can't read files\n`)
	a = additions(context.Background(), c, newLimiter(), all, msg(srv.URL+"/long", ""))
	if !strings.HasPrefix(a.Note, "[page "+srv.URL+"/long]\n```\nxxx") || !strings.Contains(a.Note, "\n[…truncated]\n```") {
		t.Fatalf("long %.80q", a.Note)
	}
	logged(`<6>link \d+\.\ds, \d+ bytes html, 15000 of 20000 chars of text \(<main>\)\n`)
	for path, want := range map[string]string{
		"/bin":     ", which isn't a page you can read]",
		"/empty":   ", a page with no text you can read]",
		"/missing": " failed to load: status 404]",
		"/loop":    " failed to load: too many or bad redirects]",
	} {
		if a := additions(context.Background(), c, newLimiter(), all, msg(srv.URL+path, "")); !strings.HasSuffix(a.Note, want) || a.HistoryNote != a.Note {
			t.Errorf("%s: %q / %q", path, a.Note, a.HistoryNote)
		}
	}
	logged(`<6>link \d+\.\ds, 2 bytes application/zip, not a page the bot reads\n`)
	logged(`<4>link \d+\.\ds, \d+ bytes html, no readable text\n`)
	logged(`<4>link failed after \d+\.\ds: status 404\n`)
	logged(`<4>link failed after \d+\.\ds: too many or bad redirects\n`)
	if a := additions(context.Background(), c, newLimiter(), all, msg("no links here", "")); a.Note != "" {
		t.Fatalf("no links: %+v", a)
	}
	for _, s := range []string{srv.URL, "127.0.0.1", "Gold", "evil", "xxx"} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("%q reached the log: %q", s, logs.String())
		}
	}
}

// A huge page is read as a stream: memory stays small and the text stops
// near the cap.
func TestPageTextIsStreamed(t *testing.T) {
	page := []byte("<html><head><title>Big</title></head><body>" + strings.Repeat("<p>a", 1_300_000) + "</body></html>")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	title, text, _ := pageText(page, "text/html")
	runtime.ReadMemStats(&after)
	if title != "Big" || len(text) > maxText*2+10 {
		t.Fatalf("title %q, %d bytes", title, len(text))
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 30<<20 {
		t.Fatalf("allocated %d MB", n>>20)
	}
}

// A page's first article or main element stands for it when it has
// minArticle characters, so a menu of plain divs is left out; a short one,
// or none, gives the whole page. A nested article stays in its outer one.
// The log is told which.
func TestArticleText(t *testing.T) {
	menu := "<div class=menu>" + strings.Repeat("Menu item ", 30) + "</div>"
	story := strings.Repeat("The story goes on. ", 22)
	for _, tc := range []struct {
		page, want, from string
	}{
		{menu + "<article><p>" + story + "</p></article>", strings.TrimSpace(story), "<article>"},
		{menu + "<main><p>Short.</p></main>", strings.TrimSpace(strings.Repeat("Menu item ", 30)) + "\nShort.", "whole page"},
		{menu + "<article><p>" + story + "</p><article><p>Inner.</p></article><p>Tail.</p></article><article>Second</article>",
			strings.TrimSpace(story) + "\nInner.\nTail.", "<article>"},
		{menu + "<main><p>" + story + "</p></main>", strings.TrimSpace(story), "<main>"},
		{"<p>No article here.</p>", "No article here.", "whole page"},
	} {
		if _, got, from := pageText([]byte("<html><body>"+tc.page+"</body></html>"), "text/html"); got != tc.want || from != tc.from {
			t.Errorf("got %q from %s\nwant %q from %s", got, from, tc.want, tc.from)
		}
	}
}

func newLimiter() *limiter { return &limiter{recent: map[string][]time.Time{}} }

// Links that look private or one-time are not fetched; ordinary ones are.
func TestPrivate(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://news.example.com/article?id=123":                   false,
		"https://youtube.com/watch?v=x":                             false,
		"https://example.com/keyboards?keyword=go&sortkey=price":    false,
		"https://example.com/":                                      false,
		"https://example.com/reset-your-thinking-article":           false, // a whole segment only
		"https://react.dev/reference/react/hooks":                   false,
		"https://example.com/login":                                 false, // harmless to open signed out
		"https://shop.example.com/promo?code=AU&session=x&hash=y":   false,
		"https://example.com/auth/callback?state=x":                 false,
		"https://app.example.com/reset?token=abc":                   true,
		"https://app.example.com/reset/abc123":                      true,
		"https://maker.ifttt.com/trigger/x/with/key/abc":            true,
		"https://example.com/unsubscribe/abc":                       true,
		"https://example.com/Unsubscribe/abc":                       true,
		"https://bucket.s3.amazonaws.com/f.pdf?X-Amz-Signature=abc": true,
		"https://api.example.com/v1/quote?API_KEY=abc":              true,
		"https://maps.example.com/embed?key=abc":                    true,
		"https://example.com/data?access-token=abc":                 true,
		"https://example.com/api/webhooks/123":                      true,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := private(u); got != want {
			t.Errorf("private(%s) = %v", raw, got)
		}
	}
}

// A private-looking link isn't fetched, and neither is one a redirect
// points at: the server never sees either. Both are logged, without the
// URL.
func TestPrivateLinksAreNotFetched(t *testing.T) {
	logs := capture(t)
	allowLoopback = true
	defer func() { allowLoopback = false }()
	var mu sync.Mutex
	var hits []string
	hit := func(r *http.Request) { mu.Lock(); hits = append(hits, r.URL.Path); mu.Unlock() }
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { hit(r) })
	mux.HandleFunc("/short", func(w http.ResponseWriter, r *http.Request) {
		hit(r)
		http.Redirect(w, r, "/reset?token=x", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newClient()
	a := additions(context.Background(), c, newLimiter(), feature.Info{}, msg("is this legit? "+srv.URL+"/login?token=abc", ""))
	if a.Note != "[link "+srv.URL+"/login?token=abc not fetched: it looks like a private or one-time link]" || a.HistoryNote != a.Note {
		t.Fatalf("note %q", a.Note)
	}
	a = additions(context.Background(), c, newLimiter(), feature.Info{}, msg(srv.URL+"/short", ""))
	if a.Note != "[link "+srv.URL+"/short failed to load: redirected to a private or one-time link]" {
		t.Fatalf("redirect note %q", a.Note)
	}
	if l := logs.String(); !strings.Contains(l, "<6>link not fetched: looks private or one-time\n") ||
		!regexp.MustCompile(`<4>link failed after \d+\.\ds: redirected to a private or one-time link\n`).MatchString(l) ||
		strings.Contains(l, srv.URL) || strings.Contains(l, "token") {
		t.Fatalf("log %q", l)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(hits, []string{"/short"}) {
		t.Fatalf("server saw %v", hits)
	}
}

// Each chat can have maxPerHour links fetched an hour; another chat is
// counted apart, and an hour later the first can again. Over the cap, the
// model is told why, and the log, without the URL.
func TestFetchesPerHour(t *testing.T) {
	logs := capture(t)
	defer func(f func() time.Time) { now = f }(now)
	clock := time.Now()
	now = func() time.Time { return clock }
	l := newLimiter()
	for i := range maxPerHour {
		if !l.allow("g:a") {
			t.Fatalf("fetch %d refused", i+1)
		}
		clock = clock.Add(time.Minute)
	}
	if l.allow("g:a") {
		t.Fatal("fetch over the cap allowed")
	}
	if !l.allow("g:b") {
		t.Fatal("another chat refused")
	}
	clock = clock.Add(time.Hour)
	if !l.allow("g:a") {
		t.Fatal("refused an hour later")
	}
	if _, kept := l.recent["g:b"]; kept {
		t.Fatal("an idle chat was kept")
	}
	// Over the cap, the model is told why.
	for range maxPerHour {
		l.allow("u:c")
	}
	a := additions(context.Background(), newClient(), l, feature.Info{}, feature.Message{Chat: feature.Chat{User: "c"}, Text: "https://example.com/a", Addressed: true})
	if a.Note != "[link https://example.com/a not fetched: too many links this hour; try again later]" {
		t.Fatalf("note %q", a.Note)
	}
	if l := logs.String(); l != "<4>link not fetched: 20 an hour for this chat reached\n" {
		t.Fatalf("log %q", l)
	}
}
