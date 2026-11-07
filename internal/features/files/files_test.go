package files

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
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

var all = feature.Info{Images: true, Audio: true, PDFs: true}

// A PDF and audio go to the model as files; history keeps only notes, saying
// they aren't kept. Images and stickers are left to vision. Each file is
// logged by kind and size, never its name or type.
func TestPDFAndAudio(t *testing.T) {
	var out, errw logx.Buffer
	defer logx.SetOutput(&out, &errw, true)()
	pdf, voice := []byte("%PDF-1.7\n..."), []byte("\x00\x00\x00\x1cftypM4A ")
	h := host(t, all, map[string][]byte{"p": pdf, "v": voice})
	a := additionsOf(t, h, turn(feature.Media{ID: "img"}, feature.Media{ID: "p", File: true, Name: "report.pdf", MIME: "application/pdf"},
		feature.Media{ID: "v", File: true, Name: "", MIME: "audio/aac"}, feature.Media{ID: "s", Sticker: true}))
	if a.Note != `[file "report.pdf" attached]`+"\n"+`[file "untitled" attached]` {
		t.Fatalf("note %q", a.Note)
	}
	if a.HistoryNote != `[file "report.pdf" was shown to you`+feature.NotKept(1)+"]\n"+`[file "untitled" was shown to you`+feature.NotKept(1)+"]" {
		t.Fatalf("history %q", a.HistoryNote)
	}
	if len(a.Files) != 2 || a.Files[0].Name != "report.pdf" || a.Files[0].MIME != "application/pdf" || !bytes.Equal(a.Files[0].Data, pdf) || a.Files[1].MIME != "audio/aac" || a.Images != nil || !a.Content {
		t.Fatalf("files %+v", a.Files)
	}
	first := "<6>file pdf 12 bytes, attached\n<6>file audio 12 bytes, attached\n"
	if out.String() != first || errw.String() != "" {
		t.Fatalf("log %q / %q", out.String(), errw.String())
	}
	// A model that takes neither is told so; nothing is sent.
	h = host(t, feature.Info{}, map[string][]byte{"p": pdf, "v": voice})
	a = additionsOf(t, h, turn(feature.Media{ID: "p", File: true, Name: "report.pdf"}, feature.Media{ID: "v", File: true, Name: "note.m4a"}))
	if a.Note != `[file "report.pdf", a PDF, which you can't read with this model]`+"\n"+
		`[file "note.m4a", audio such as a voice note, which you can't hear with this model]` || a.Files != nil || a.HistoryNote != a.Note || a.Content {
		t.Fatalf("got %+v", a)
	}
	if l := out.String(); l != first+"<6>file pdf 12 bytes, not sent: this model can't read PDFs\n<6>file audio 12 bytes, not sent: this model can't hear audio\n" ||
		strings.Contains(l, "report") || strings.Contains(l, "m4a") || strings.Contains(l, "application/pdf") {
		t.Fatalf("log %q", l)
	}
}

// Text is shown inline, capped, in a fence its own backticks can't close;
// history keeps only the note. Text that isn't valid UTF-8, or binary, is
// a file the model can't read. The log has each file's size and how much
// text was shown (without the newlines the fence trims), or the kind of
// content it couldn't read.
func TestText(t *testing.T) {
	var out, errw logx.Buffer
	defer logx.SetOutput(&out, &errw, true)()
	long := strings.Repeat("x", maxText+10)
	h := host(t, all, map[string][]byte{"t": []byte("a ``` b\n"), "long": []byte(long), "bad": {0xff, 0xfe, 'a'}, "bin": []byte("a\x00b")})
	a := additionsOf(t, h, turn(feature.Media{ID: "t", File: true, Name: "notes.md", MIME: "text/markdown"}))
	if a.Note != `[file "notes.md" attached; its text follows]`+"\n````\na ``` b\n````" || a.HistoryNote != `[file "notes.md" was shown to you`+feature.NotKept(1)+"]" || a.Files != nil || !a.Content {
		t.Fatalf("got %+v", a)
	}
	a = additionsOf(t, h, turn(feature.Media{ID: "long", File: true, Name: "big.log"}))
	if !strings.Contains(a.Note, "\n[…truncated]\n```") || len(a.Note) > maxText+200 {
		t.Fatalf("long note is %d bytes", len(a.Note))
	}
	for _, id := range []string{"bad", "bin"} {
		if a := additionsOf(t, h, turn(feature.Media{ID: id, File: true, Name: "x.txt", MIME: "text/plain"})); a.Note != `[file "x.txt", which can't be read]` {
			t.Errorf("%s: %q", id, a.Note)
		}
	}
	want := "<6>file text 8 bytes, 7 chars of text\n<6>file text 50010 bytes, 50000 of 50010 chars of text\n" +
		"<6>file 3 bytes text/plain; charset=utf-8, not a kind the bot reads\n<6>file 3 bytes application/octet-stream, not a kind the bot reads\n"
	if out.String() != want || errw.String() != "" {
		t.Fatalf("log %q / %q", out.String(), errw.String())
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
	named := officeFile(t, map[string]string{
		"xl/workbook.xml": `<workbook><sheets><sheet name="Budget  2030" sheetId="1" r:id="rId2"/><sheet name="" sheetId="2" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/>` +
			`<Relationship Id="rId2" Target="/xl/worksheets/sheet2.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c r="A1"><v>1</v></c></row></sheetData></worksheet>`,
		"xl/worksheets/sheet2.xml": `<worksheet><sheetData><row><c r="A1"><v>2</v></c></row></sheetData></worksheet>`,
	})
	pptx := officeFile(t, map[string]string{
		"ppt/slides/slide10.xml": `<p:sld><a:p><a:r><a:t>Ten</a:t></a:r></a:p></p:sld>`,
		"ppt/slides/slide2.xml":  `<p:sld><a:p><a:r><a:t>Two</a:t></a:r></a:p></p:sld>`,
	})
	for name, want := range map[string]string{
		"a.docx": "Hello\tworld\nSecond\n",
		"b.xlsx": "Sheet 1:\nName\t\tPrice\nAAPL\t\t187.5\nSheet 2:\n\tinline\n",
		"c.pptx": "Slide 1:\nTwo\nSlide 2:\nTen\n",
		"d.xlsx": "Sheet 1:\n1\nSheet \"Budget 2030\":\n2\n", // named from the workbook, unnamed by number
	} {
		data := map[string][]byte{"a.docx": docx, "b.xlsx": xlsx, "c.pptx": pptx, "d.xlsx": named}[name]
		got, _, err := officeText(data, name[strings.Index(name, "."):])
		if err != nil || got != want {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	// A file is logged by kind and size with how much text it gave (what
	// the fence shows; past the cap, "first 50000", since the document's
	// length is unknown), or the kind of error, never the error's text,
	// which can quote the document.
	var out, errw logx.Buffer
	defer logx.SetOutput(&out, &errw, true)()
	broken := []byte("PK\x03\x04 not really")
	big := officeFile(t, map[string]string{"word/document.xml": "<w:document><w:body><w:p><w:r><w:t>" + strings.Repeat("a", maxText+10) + "</w:t></w:r></w:p></w:body></w:document>"})
	files := map[string][]byte{"d": docx, "big": big, "broken": broken, "empty": officeFile(t, map[string]string{"word/document.xml": "<w:document/>"}),
		"missing": officeFile(t, map[string]string{"xl/foo.xml": "<x/>"}), "badxml": officeFile(t, map[string]string{"word/document.xml": "<w:document><w:p>"})}
	h := host(t, all, files)
	if a := additionsOf(t, h, turn(feature.Media{ID: "d", File: true, Name: "letter.docx"})); a.Note != `[file "letter.docx" attached; its text follows]`+"\n```\nHello\tworld\nSecond\n```" || !a.Content {
		t.Fatalf("note %q", a.Note)
	}
	if a := additionsOf(t, h, turn(feature.Media{ID: "big", File: true, Name: "big.docx"})); !strings.HasSuffix(a.Note, "\n[…truncated]\n```") ||
		!strings.HasPrefix(a.Note, `[file "big.docx" attached; only the first part of its text is shown, as the file is too big to read whole]`) {
		t.Fatalf("big note %.60q", a.Note)
	}
	if want := fmt.Sprintf("<6>file docx %d bytes, 18 chars of text\n<6>file docx %d bytes, first %d chars of text\n", len(docx), len(big), maxText); out.String() != want {
		t.Fatalf("log %q", out.String())
	}
	for id, kind := range map[string]string{"broken": "not a zip", "empty": "no text", "missing": "missing part", "badxml": "bad xml"} {
		before := errw.String()
		if a := additionsOf(t, h, turn(feature.Media{ID: id, File: true, Name: "x.docx"})); a.Note != `[file "x.docx", which can't be read]` || a.Content {
			t.Errorf("%s: %q", id, a.Note)
		}
		if l := strings.TrimPrefix(errw.String(), before); l != fmt.Sprintf("<4>could not read an Office file (docx, %d bytes): %s\n", len(files[id]), kind) {
			t.Errorf("%s: log %q", id, l)
		}
	}
	if k := errKind(errors.New("x")); k != "unreadable" {
		t.Errorf("errKind of an unknown error: %q", k)
	}
	if l := out.String() + errw.String(); strings.Contains(l, "w:") || strings.Contains(l, "docx\"") || strings.Contains(l, "x.docx") {
		t.Errorf("log %q", l)
	}
}

// The budgets a crafted file meets: a zip whose end record declares more
// than maxParts entries, or that has them, is refused before its directory
// is built; elements nested past maxDepth stop the read; parts inflating
// to more than maxTotal together stop it, however little text they hold,
// and what was read by then stands, cut. Each refusal is reported by kind.
// The budgets are lowered for the test, so the same paths run on a
// sixteenth of the data.
func TestOfficeBudgets(t *testing.T) {
	smallBudgets(t)
	declared := officeFile(t, map[string]string{"word/document.xml": "<w:document/>"})
	binary.LittleEndian.PutUint16(declared[bytes.LastIndex(declared, []byte("PK\x05\x06"))+10:], 0xffff)
	if n := entries(declared); n != 0xffff {
		t.Fatalf("entries %d", n)
	}
	many := map[string]string{"word/document.xml": "<w:document/>"}
	for i := range maxParts {
		many[fmt.Sprintf("x/%d", i)] = ""
	}
	deep := "<w:document>" + strings.Repeat("<w:p>", maxDepth+50) + "hi" + strings.Repeat("</w:p>", maxDepth+50) + "</w:document>"
	blank := "<p:sld>" + strings.Repeat(" ", maxPart-100) + "</p:sld>" // inflates to maxPart, no text
	slides := map[string]string{}
	for i := range maxTotal/maxPart + 2 {
		slides[fmt.Sprintf("ppt/slides/slide%d.xml", i+1)] = blank
	}
	cases := map[string]struct {
		data []byte
		ext  string
		err  error
		kind string
	}{
		"declared": {declared, ".docx", errParts, "too many parts"},
		"many":     {storedZip(t, many), ".docx", errParts, "too many parts"},
		"deep":     {officeFile(t, map[string]string{"word/document.xml": deep}), ".docx", errDeep, "too deep"},
	}
	for name, tc := range cases {
		if _, _, err := officeText(tc.data, tc.ext); !errors.Is(err, tc.err) || errKind(err) != tc.kind {
			t.Errorf("%s: %v (%s)", name, err, errKind(err))
		}
	}
	if n := entries(cases["many"].data); n != maxParts+1 {
		t.Errorf("entries %d", n)
	}
	// Slides of XML with no text spend the budget; the slides read by then
	// stand, cut, so a deck heavy with shapes is still read as far as it
	// goes: here three whole ones and the start of the fourth, not the fifth.
	if text, cut, err := officeText(storedZip(t, slides), ".pptx"); err != nil || !cut || !strings.HasPrefix(text, "Slide 1:\n") ||
		!strings.Contains(text, fmt.Sprintf("Slide %d:", maxTotal/maxPart+1)) || strings.Contains(text, fmt.Sprintf("Slide %d:", maxTotal/maxPart+2)) {
		t.Errorf("size: %q, cut %v, %v", text, cut, err)
	}
	// The budget is spent part by part: a part reads what is left of it, one
	// that fits exactly is read whole, and one that needs more is refused.
	small := officeFile(t, map[string]string{"word/document.xml": "<w:document/>"})
	z, err := zip.NewReader(bytes.NewReader(small), int64(len(small)))
	if err != nil {
		t.Fatal(err)
	}
	budget := 20
	if next, err := part(z, "word/document.xml", &budget); err != nil || budget != 20-len("<w:document/>") || next == nil {
		t.Fatalf("part: %v, budget %d", err, budget)
	}
	if _, err := part(z, "word/document.xml", &budget); !errors.Is(err, errSize) || budget != 20-len("<w:document/>") {
		t.Fatalf("part over the budget: %v, budget %d", err, budget)
	}
	budget = len("<w:document/>")
	if _, err := part(z, "word/document.xml", &budget); err != nil || budget != 0 {
		t.Fatalf("part filling the budget: %v, budget %d", err, budget)
	}
	if _, err := part(z, "word/document.xml", &budget); !errors.Is(err, errSize) {
		t.Fatalf("part with no budget: %v", err)
	}
}

// storedZip is a zip of parts written uncompressed, quick to build however
// many or large they are.
func storedZip(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for name, content := range parts {
		w, err := z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	z.Close()
	return b.Bytes()
}

// smallBudgets lowers the Office budgets for the test to a sixteenth or so,
// restoring them after.
func smallBudgets(t *testing.T) {
	part, total, depth, parts := maxPart, maxTotal, maxDepth, maxParts
	maxPart, maxTotal, maxDepth, maxParts = 640<<10, 2<<20, 64, 2_000
	t.Cleanup(func() { maxPart, maxTotal, maxDepth, maxParts = part, total, depth, parts })
}

// The budgets are what the docs say.
func TestOfficeBudgetValues(t *testing.T) {
	if maxPart != 10<<20 || maxTotal != 32<<20 || maxDepth != 256 || maxParts != 20_000 {
		t.Fatalf("budgets %d, %d, %d, %d", maxPart, maxTotal, maxDepth, maxParts)
	}
}

// A zip that inflates past maxPart is read only that far.
func TestOfficeBomb(t *testing.T) {
	smallBudgets(t)
	huge := "<w:document><w:body><w:p><w:r><w:t>" + strings.Repeat("a", maxPart+1000) + "</w:t></w:r></w:p></w:body></w:document>"
	got, _, _ := officeText(officeFile(t, map[string]string{"word/document.xml": huge}), ".docx")
	if len(got) > maxText+maxPart/100 {
		t.Fatalf("read %d bytes", len(got))
	}
}

// A file that can't be fetched, one no longer available (a quoted file not
// remembered), and one of another kind are each noted and logged, without
// a name; a name is made safe.
func TestNotes(t *testing.T) {
	var out, errw logx.Buffer
	defer logx.SetOutput(&out, &errw, true)()
	h := host(t, all, map[string][]byte{"zip": []byte("PK\x03\x04...")})
	a := additionsOf(t, h, turn(feature.Media{ID: "gone", File: true, Name: "a.pdf"}, feature.Media{File: true, Quoted: true, Name: "b.pdf"},
		feature.Media{ID: "zip", File: true, Name: "c.zip\n]\"injected"}))
	want := `[file "a.pdf", which couldn't be fetched]` + "\n" + `[file "b.pdf", which is no longer available]` + "\n" + `[file "c.zip ]\"injected", which can't be read]`
	if a.Note != want || a.HistoryNote != want || a.Files != nil || a.Content {
		t.Fatalf("got %+v", a)
	}
	if errw.String() != "<4>could not fetch a file: timed out\n<4>file no longer available\n" || out.String() != "<6>file 7 bytes application/zip, not a kind the bot reads\n" {
		t.Fatalf("log %q / %q", errw.String(), out.String())
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
	got, _, err := officeText(xlsx, ".xlsx")
	if err != nil || len(got) > 3*maxText {
		t.Fatalf("xlsx gave %d bytes, %v", len(got), err)
	}
	slides := map[string]string{}
	for i := range 500 {
		slides[fmt.Sprintf("ppt/slides/slide%d.xml", i+1)] = "<p:sld><a:p><a:r><a:t>" + strings.Repeat("y", 1000) + "</a:t></a:r></a:p></p:sld>"
	}
	if got, _, err := officeText(officeFile(t, slides), ".pptx"); err != nil || len(got) > 3*maxText || strings.Contains(got, "Slide 100:") {
		t.Fatalf("pptx gave %d bytes, %v", len(got), err)
	}
}
