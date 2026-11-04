package links

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

func msg(text, quote string) feature.Message {
	m := feature.Message{Chat: feature.Chat{User: "u"}, Text: text, Addressed: true}
	if quote != "" {
		m.Quote = &feature.Quote{Text: quote}
	}
	return m
}

// URLs come from the text, then the quote, distinct, at most two, with
// sentence punctuation and unbalanced brackets trimmed.
func TestURLs(t *testing.T) {
	for _, tc := range []struct {
		text, quote string
		want        []string
	}{
		{"see https://example.com/a.", "", []string{"https://example.com/a"}},
		{"(https://en.wikipedia.org/wiki/Go_(game)) and [x](https://x.org/y)", "", []string{"https://en.wikipedia.org/wiki/Go_(game)", "https://x.org/y"}},
		{"<https://a.com/>, https://a.com/ again", "", []string{"https://a.com/"}},
		{"summarise", "read http://b.org/p?q=1#top please", []string{"http://b.org/p?q=1"}},
		{"https://1.com https://2.com https://3.com", "", []string{"https://1.com", "https://2.com"}},
		{"ftp://x.com mailto:a@b.c https://user:pw@c.com/ http://", "", nil},
	} {
		if got := urls(msg(tc.text, tc.quote)); !slices.Equal(got, tc.want) {
			t.Errorf("%q: %q, want %q", tc.text, got, tc.want)
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

func capture(t *testing.T) *logx.Buffer {
	var b logx.Buffer
	t.Cleanup(logx.SetOutput(&b, &b, false))
	return &b
}

// A page's article text and title are shown, without its navigation or
// scripts; history keeps a note. Plain text is shown as it is, a PDF as a
// file, and anything else, an error status or too many redirects as a note.
func TestFetch(t *testing.T) {
	capture(t)
	allowLoopback = true
	defer func() { allowLoopback = false }()
	article := strings.Repeat("Gold rose today. ", 20)
	mux := http.NewServeMux()
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

	a := additions(context.Background(), c, all, msg("what's this "+srv.URL+"/page?", ""))
	if !strings.HasPrefix(a.Note, "[page "+srv.URL+`/page: "Gold news"]`+"\n```\nGold\nGold rose today.") ||
		strings.Contains(a.Note, "evil") || strings.Contains(a.Note, "Home | About") || strings.Contains(a.Note, "©") {
		t.Fatalf("note %q", a.Note)
	}
	if a.HistoryNote != "[page "+srv.URL+"/page was shown to you; it isn't kept]" {
		t.Fatalf("history %q", a.HistoryNote)
	}
	if a := additions(context.Background(), c, all, msg(srv.URL+"/latin1", "")); a.Note != "[page "+srv.URL+"/latin1]\n```\ncafé\n```" {
		t.Fatalf("latin-1 %q", a.Note)
	}
	a = additions(context.Background(), c, all, msg("", "read "+srv.URL+"/report.pdf"))
	if a.Note != "[page "+srv.URL+"/report.pdf, a PDF, attached]" || len(a.Files) != 1 || a.Files[0].Name != "report.pdf" || a.Files[0].MIME != "application/pdf" {
		t.Fatalf("pdf %+v", a)
	}
	if a := additions(context.Background(), c, feature.Info{}, msg(srv.URL+"/report.pdf", "")); a.Files != nil || !strings.Contains(a.Note, "which you can't read with this model") {
		t.Fatalf("pdf without files %+v", a)
	}
	for path, want := range map[string]string{
		"/bin":     ", which isn't a page you can read]",
		"/missing": " failed to load: status 404]",
		"/loop":    " failed to load: too many or bad redirects]",
	} {
		if a := additions(context.Background(), c, all, msg(srv.URL+path, "")); !strings.HasSuffix(a.Note, want) || a.HistoryNote != a.Note {
			t.Errorf("%s: %q / %q", path, a.Note, a.HistoryNote)
		}
	}
	if a := additions(context.Background(), c, all, msg("no links here", "")); a.Note != "" {
		t.Fatalf("no links: %+v", a)
	}
}
