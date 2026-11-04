package files

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// host serves each file by its Media ID from data, for a model that takes
// what info says.
func host(t *testing.T, info feature.Info, data map[string][]byte) *featuretest.Host {
	t.Helper()
	h := featuretest.New()
	h.Bot = info
	h.Fetch = func(m feature.Message, i int) ([]byte, error) {
		if d, ok := data[m.Media[i].ID]; ok {
			return d, nil
		}
		return nil, errors.New("timed out")
	}
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	return h
}

func turn(media ...feature.Media) feature.Message {
	return feature.Message{Chat: feature.Chat{User: "u-alice"}, Sender: "u-alice", Name: "Alice", Text: "look", Media: media, Addressed: true}
}

func additionsOf(t *testing.T, h *featuretest.Host, m feature.Message) feature.Additions {
	t.Helper()
	as := h.Turn(context.Background(), m)
	if len(as) != 1 {
		t.Fatalf("%d OnTurn hooks", len(as))
	}
	return as[0]
}

// officeFile is a zip holding parts, as an Office file is.
func officeFile(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for name, body := range parts {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	z.Close()
	return b.Bytes()
}

var all = feature.Info{Images: true, Audio: true, Files: true}

// A PDF and audio go to the model as files; history keeps only notes, saying
// they aren't kept. Images and stickers are left to vision.
func TestPDFAndAudio(t *testing.T) {
	pdf, voice := []byte("%PDF-1.7\n..."), []byte("\x00\x00\x00\x1cftypM4A ")
	h := host(t, all, map[string][]byte{"p": pdf, "v": voice})
	a := additionsOf(t, h, turn(feature.Media{ID: "img"}, feature.Media{ID: "p", File: true, Name: "report.pdf", MIME: "application/pdf"},
		feature.Media{ID: "v", File: true, Name: "", MIME: "audio/aac"}, feature.Media{ID: "s", Sticker: true}))
	if a.Note != `[file "report.pdf" attached]`+"\n"+`[file "untitled" attached]` {
		t.Fatalf("note %q", a.Note)
	}
	if a.HistoryNote != `[file "report.pdf" attached`+notKept+"]\n"+`[file "untitled" attached`+notKept+"]" {
		t.Fatalf("history %q", a.HistoryNote)
	}
	if len(a.Files) != 2 || a.Files[0].Name != "report.pdf" || a.Files[0].MIME != "application/pdf" || !bytes.Equal(a.Files[0].Data, pdf) || a.Files[1].MIME != "audio/aac" || a.Images != nil {
		t.Fatalf("files %+v", a.Files)
	}
	// A model that takes neither is told so; nothing is sent.
	h = host(t, feature.Info{}, map[string][]byte{"p": pdf, "v": voice})
	a = additionsOf(t, h, turn(feature.Media{ID: "p", File: true, Name: "report.pdf"}, feature.Media{ID: "v", File: true, Name: "note.m4a"}))
	if a.Note != `[file "report.pdf", a PDF, which you can't read with this model]`+"\n"+
		`[file "note.m4a", audio such as a voice note, which you can't hear with this model]` || a.Files != nil || a.HistoryNote != a.Note {
		t.Fatalf("got %+v", a)
	}
}

// Text is shown inline, capped, in a fence its own backticks can't close;
// history keeps only the note. Text that isn't valid UTF-8, or binary, is
// a file the model can't read.
func TestText(t *testing.T) {
	long := strings.Repeat("x", maxText+10)
	h := host(t, all, map[string][]byte{"t": []byte("a ``` b\n"), "long": []byte(long), "bad": {0xff, 0xfe, 'a'}, "bin": []byte("a\x00b")})
	a := additionsOf(t, h, turn(feature.Media{ID: "t", File: true, Name: "notes.md", MIME: "text/markdown"}))
	if a.Note != `[file "notes.md" attached, its text]`+"\n````\na ``` b\n````" || a.HistoryNote != `[file "notes.md" attached, its text`+notKept+"]" || a.Files != nil {
		t.Fatalf("got %+v", a)
	}
	a = additionsOf(t, h, turn(feature.Media{ID: "long", File: true, Name: "big.log"}))
	if !strings.Contains(a.Note, "\n[…truncated]\n```") || len(a.Note) > maxText+200 {
		t.Fatalf("long note is %d bytes", len(a.Note))
	}
	for _, id := range []string{"bad", "bin"} {
		if a := additionsOf(t, h, turn(feature.Media{ID: id, File: true, Name: "x.txt", MIME: "text/plain"})); a.Note != `[file "x.txt", which you can't read]` {
			t.Errorf("%s: %q", id, a.Note)
		}
	}
}

// docx, xlsx and pptx are shown as their text: paragraphs, cells in their
// columns by sheet, and slides in order.
func TestOffice(t *testing.T) {
	docx := officeFile(t, map[string]string{"word/document.xml": `<w:document xmlns:w="w"><w:body>` +
		`<w:p><w:r><w:t>Hello</w:t></w:r><w:r><w:tab/><w:t>world</w:t></w:r></w:p><w:p><w:r><w:t>Second</w:t></w:r></w:p></w:body></w:document>`})
	xlsx := officeFile(t, map[string]string{
		"xl/sharedStrings.xml":     `<sst><si><t>Name</t></si><si><t>Price</t></si><si><r><t>AA</t></r><r><t>PL</t></r></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c r="A1" t="s"><v>0</v></c><c r="C1" t="s"><v>1</v></c></row><row><c r="A2" t="s"><v>2</v></c><c r="C2"><v>187.5</v></c></row></sheetData></worksheet>`,
		"xl/worksheets/sheet2.xml": `<worksheet><sheetData><row><c r="B1" t="inlineStr"><is><t>inline</t></is></c></row></sheetData></worksheet>`,
	})
	pptx := officeFile(t, map[string]string{
		"ppt/slides/slide10.xml": `<p:sld><a:p><a:r><a:t>Ten</a:t></a:r></a:p></p:sld>`,
		"ppt/slides/slide2.xml":  `<p:sld><a:p><a:r><a:t>Two</a:t></a:r></a:p></p:sld>`,
	})
	for name, want := range map[string]string{
		"a.docx": "Hello\tworld\nSecond\n",
		"b.xlsx": "Sheet 1:\nName\t\tPrice\nAAPL\t\t187.5\nSheet 2:\n\tinline\n",
		"c.pptx": "Slide 1:\nTwo\nSlide 2:\nTen\n",
	} {
		data := map[string][]byte{"a.docx": docx, "b.xlsx": xlsx, "c.pptx": pptx}[name]
		got, err := officeText(data, name[strings.Index(name, "."):])
		if err != nil || got != want {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	h := host(t, all, map[string][]byte{"d": docx, "broken": []byte("PK\x03\x04 not really"), "empty": officeFile(t, map[string]string{"word/document.xml": "<w:document/>"})})
	if a := additionsOf(t, h, turn(feature.Media{ID: "d", File: true, Name: "letter.docx"})); a.Note != `[file "letter.docx" attached, its text]`+"\n```\nHello\tworld\nSecond\n```" {
		t.Fatalf("note %q", a.Note)
	}
	var out, errw logx.Buffer
	defer logx.SetOutput(&out, &errw, true)()
	for _, id := range []string{"broken", "empty"} {
		if a := additionsOf(t, h, turn(feature.Media{ID: id, File: true, Name: "x.docx"})); a.Note != `[file "x.docx", which couldn't be read]` {
			t.Errorf("%s: %q", id, a.Note)
		}
	}
	if !strings.Contains(errw.String(), "could not read an Office file") {
		t.Errorf("log %q", errw.String())
	}
}

// A zip that inflates past maxPart is read only that far.
func TestOfficeBomb(t *testing.T) {
	huge := "<w:document><w:body><w:p><w:r><w:t>" + strings.Repeat("a", maxPart+1000) + "</w:t></w:r></w:p></w:body></w:document>"
	got, _ := officeText(officeFile(t, map[string]string{"word/document.xml": huge}), ".docx")
	if len(got) > maxText+maxPart/100 {
		t.Fatalf("read %d bytes", len(got))
	}
}

// A file that can't be fetched, one no longer available (a quoted file not
// remembered), and one of another kind are each noted; a name is made safe.
func TestNotes(t *testing.T) {
	var out, errw logx.Buffer
	defer logx.SetOutput(&out, &errw, true)()
	h := host(t, all, map[string][]byte{"zip": []byte("PK\x03\x04...")})
	a := additionsOf(t, h, turn(feature.Media{ID: "gone", File: true, Name: "a.pdf"}, feature.Media{File: true, Quoted: true, Name: "b.pdf"},
		feature.Media{ID: "zip", File: true, Name: "c.zip\n]\"injected"}))
	want := `[file "a.pdf", which failed to load]` + "\n" + `[file "b.pdf", which is no longer available]` + "\n" + `[file "c.zip ]\"injected", which you can't read]`
	if a.Note != want || a.HistoryNote != want || a.Files != nil {
		t.Fatalf("got %q", a.Note)
	}
	if !strings.Contains(errw.String(), "could not fetch a file: timed out") || strings.Contains(errw.String(), "a.pdf") {
		t.Fatalf("log %q", errw.String())
	}
	if a := additionsOf(t, h, turn(feature.Media{ID: "img"})); a.Note != "" || a.Files != nil {
		t.Fatalf("no files: %+v", a)
	}
}

// Many cells of one long shared string, or many slides, stop at the cap:
// a small file can't inflate into a huge text.
func TestOfficeAmplification(t *testing.T) {
	cells := strings.Repeat(`<c t="s"><v>0</v></c>`, 20_000)
	xlsx := officeFile(t, map[string]string{
		"xl/sharedStrings.xml":     "<sst><si><t>" + strings.Repeat("x", maxText) + "</t></si></sst>",
		"xl/worksheets/sheet1.xml": "<worksheet><sheetData><row>" + cells + "</row><row>" + cells + "</row></sheetData></worksheet>",
	})
	got, err := officeText(xlsx, ".xlsx")
	if err != nil || len(got) > 3*maxText {
		t.Fatalf("xlsx gave %d bytes, %v", len(got), err)
	}
	slides := map[string]string{}
	for i := range 500 {
		slides[fmt.Sprintf("ppt/slides/slide%d.xml", i+1)] = "<p:sld><a:p><a:r><a:t>" + strings.Repeat("y", 1000) + "</a:t></a:r></a:p></p:sld>"
	}
	if got, err := officeText(officeFile(t, slides), ".pptx"); err != nil || len(got) > 3*maxText || strings.Contains(got, "Slide 100:") {
		t.Fatalf("pptx gave %d bytes, %v", len(got), err)
	}
}
