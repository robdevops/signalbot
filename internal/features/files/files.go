// Package files shows the model the files a message carries that aren't
// images: a PDF or audio as itself when the model's API takes it, text and
// Office documents as their text, and anything else as a note that it can't
// be read. History keeps only notes.
package files

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// maxText caps a file's text shown to the model, in characters.
const maxText = 50_000

var errEmpty = errors.New("no text")

// textTypes and textExts are the declared types and extensions read as
// text, besides text/*.
var (
	textTypes = map[string]bool{"application/json": true, "application/xml": true, "application/yaml": true,
		"application/x-yaml": true, "application/javascript": true, "application/x-sh": true, "application/sql": true}
	textExts = map[string]bool{".txt": true, ".md": true, ".csv": true, ".tsv": true, ".json": true, ".xml": true,
		".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".log": true, ".html": true, ".htm": true,
		".go": true, ".py": true, ".js": true, ".ts": true, ".java": true, ".c": true, ".h": true, ".cpp": true,
		".rs": true, ".rb": true, ".php": true, ".sh": true, ".sql": true, ".swift": true, ".kt": true, ".css": true}
	audioExts = map[string]string{".aac": "audio/aac", ".m4a": "audio/mp4", ".mp3": "audio/mpeg",
		".wav": "audio/wav", ".ogg": "audio/ogg", ".opus": "audio/ogg", ".flac": "audio/flac"}
)

// Register adds a message's files to the model's request on h. With FILES
// off, files are never fetched: the model is told only that each was
// attached, so it can say why it can't see one.
func Register(h feature.Host) error {
	off := !txt.Bool(h.Env("FILES", ""), true)
	if off {
		logx.Infof("files: off (FILES)")
	}
	h.OnTurn(func(ctx context.Context, m feature.Message) feature.Additions { return additions(ctx, h, m, off) })
	return nil
}

// additions notes each of m's files, with the history's version, and shows
// the model what it can take of each.
func additions(ctx context.Context, h feature.Host, m feature.Message, off bool) feature.Additions {
	info := h.Info()
	var notes, kept []string
	var files []feature.File
	content := false // the model gets what a file says
	for i, md := range m.Media {
		if !md.File {
			continue
		}
		o := one(ctx, h, info, m, i, off)
		notes, kept = append(notes, o.note()), append(kept, o.kept())
		content = content || o.shown()
		if o.file != nil {
			files = append(files, *o.file)
		}
	}
	if len(notes) == 0 {
		return feature.Additions{}
	}
	return feature.Additions{Note: strings.Join(notes, "\n"), HistoryNote: strings.Join(kept, "\n"), Files: files, Content: content}
}

// outcome is what a file came to: the note's subject (`file "name"`) and
// its status after that (such as " attached" or ", which can't be read"),
// and the text or file the model is shown, if any.
type outcome struct {
	subject, status string
	text            string
	file            *feature.File
}

// shown reports whether the model is shown what the file says.
func (o outcome) shown() bool { return o.text != "" || o.file != nil }

// note is the whole note the model gets, with the file's text under it.
func (o outcome) note() string {
	n := "[" + o.subject + o.status + "]"
	if o.text != "" {
		n += "\n" + txt.Fence(o.text, maxText)
	}
	return n
}

// kept is the note history keeps: never what the file said.
func (o outcome) kept() string {
	if o.shown() {
		return "[" + o.subject + " was shown to you" + feature.NotKept(1) + "]"
	}
	return "[" + o.subject + o.status + "]"
}

// one handles m.Media[i]. It logs one line, what the file came to, by kind
// and size: never its name or declared type, which the sender chose.
func one(ctx context.Context, h feature.Host, info feature.Info, m feature.Message, i int, off bool) outcome {
	md := m.Media[i]
	o := outcome{subject: "file " + strconv.Quote(cmp.Or(txt.SafeName(md.Name), "untitled"))}
	with := func(status string) outcome { o.status = status; return o }
	if off {
		logx.Infof("file not fetched: FILES is off")
		return with(" attached, but reading files is turned off")
	}
	if md.ID == "" {
		logx.Warnf("file no longer available")
		return with(", which is no longer available")
	}
	data, err := h.Media(ctx, m, i)
	if err != nil {
		logx.Warnf("could not fetch a file: %v", err)
		return with(", which couldn't be fetched")
	}
	name := cmp.Or(txt.SafeName(md.Name), "untitled")
	ext, sniffed := strings.ToLower(path.Ext(md.Name)), http.DetectContentType(data)
	kind := kindOf(md.MIME, ext, sniffed, data)
	what := kind
	if kind == "office" {
		what = ext[1:] // docx, xlsx or pptx
	}
	head := fmt.Sprintf("file %s %d bytes", what, len(data))
	switch kind {
	case "pdf":
		if !info.PDFs {
			logx.Infof("%s, not sent: this model can't read PDFs", head)
			return with(", a PDF, which you can't read with this model")
		}
		logx.Infof("%s, attached", head)
		o.file = &feature.File{Name: name, MIME: "application/pdf", Data: data}
		return with(" attached")
	case "audio":
		if !info.Audio {
			logx.Infof("%s, not sent: this model can't hear audio", head)
			return with(", audio such as a voice note, which you can't hear with this model")
		}
		mime := md.MIME
		if !strings.HasPrefix(strings.ToLower(mime), "audio/") {
			mime = cmp.Or(audioExts[ext], "audio/aac")
		}
		logx.Infof("%s, attached", head)
		o.file = &feature.File{Name: name, MIME: mime, Data: data}
		return with(" attached")
	case "office":
		text, cut, err := officeText(data, ext)
		if err != nil || strings.TrimSpace(text) == "" {
			logx.Warnf("could not read an Office file (%s, %d bytes): %s", what, len(data), errKind(cmp.Or(err, errEmpty)))
			return with(", which can't be read")
		}
		// The count is what the fence shows, without the newlines it trims.
		// officeText stops a little past the cap, or where the file's budget
		// ran out, so a cut document's length is unknown.
		n, shown := utf8.RuneCountInString(strings.Trim(text, "\n")), ""
		if cut {
			shown = "first " + strconv.Itoa(min(n, maxText)) + " chars of text"
		} else {
			shown = txt.Chars(n, maxText)
		}
		logx.Infof("%s, %s", head, shown)
		o.text = text
		if cut {
			return with(" attached; only the first part of its text is shown, as the file is too big to read whole")
		}
		return with(" attached; its text follows")
	case "text":
		logx.Infof("%s, %s", head, txt.Chars(utf8.RuneCount(bytes.Trim(data, "\n")), maxText))
		o.text = string(data)
		return with(" attached; its text follows")
	}
	logx.Infof("file %d bytes %s, not a kind the bot reads", len(data), txt.Clamp(sniffed, 40))
	return with(", which can't be read")
}

// kindOf is what a file is, by its declared type, extension and content:
// "pdf", "audio", "office", "text", or "" for anything else.
func kindOf(mime, ext, sniffed string, data []byte) string {
	mime, _, _ = strings.Cut(strings.ToLower(mime), ";")
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return "pdf"
	case strings.HasPrefix(mime, "audio/") || audioExts[ext] != "" || strings.HasPrefix(sniffed, "audio/"):
		return "audio"
	case officeExts[ext] && bytes.HasPrefix(data, []byte("PK\x03\x04")):
		return "office"
	case (strings.HasPrefix(mime, "text/") || textTypes[mime] || textExts[ext]) && utf8.Valid(data) && !bytes.ContainsRune(data, 0):
		return "text"
	}
	return ""
}
