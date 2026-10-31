package logx

import (
	"strings"
	"testing"
)

func logAll() {
	Critf("c")
	Errorf("e")
	Warnf("w")
	Noticef("n")
	Infof("i")
	Debugf("d")
}

// Under journald each line carries its priority; 5 to 7 go to stdout and 2
// to 4 to stderr.
func TestJournalLines(t *testing.T) {
	var out, errw strings.Builder
	defer SetOutput(&out, &errw, true)()
	logAll()
	if got, want := out.String(), "<5>n\n<6>i\n<7>d\n"; got != want {
		t.Errorf("stdout %q, want %q", got, want)
	}
	if got, want := errw.String(), "<2>c\n<3>e\n<4>w\n"; got != want {
		t.Errorf("stderr %q, want %q", got, want)
	}
}

// Elsewhere lines are plain, split across the same two streams.
func TestPlainLines(t *testing.T) {
	var out, errw strings.Builder
	defer SetOutput(&out, &errw, false)()
	logAll()
	if got, want := out.String(), "n\ni\nd\n"; got != want {
		t.Errorf("stdout %q, want %q", got, want)
	}
	if got, want := errw.String(), "c\ne\nw\n"; got != want {
		t.Errorf("stderr %q, want %q", got, want)
	}
}

func TestLineBreaksEscaped(t *testing.T) {
	var out strings.Builder
	defer SetOutput(&out, &out, true)()
	Infof("name %s", "Alice\n<3>forged\r")
	if got, want := out.String(), `<6>name Alice\n<3>forged\r`+"\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A recovered panic says what failed, never the panic's value.
func TestRecoverHidesThePanic(t *testing.T) {
	var out strings.Builder
	defer SetOutput(&out, &out, false)()
	func() {
		defer Recover("answering")
		panic("secret text")
	}()
	if got := out.String(); strings.Contains(got, "secret") || !strings.Contains(got, "internal error while answering") {
		t.Errorf("got %q", got)
	}
}
