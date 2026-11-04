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

const (
	// maxText caps a file's text shown to the model, in characters.
	maxText = 50_000
	// maxName caps a file's name in a note.
	maxName = 80
	// notKept ends the history's note for a file: history never keeps it.
	notKept = "; you see files only in the latest message, so to look again they must reply to it and mention you"
)

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

// Register adds a message's files to the model's request on h.
func Register(h feature.Host) error {
	h.OnTurn(func(ctx context.Context, m feature.Message) feature.Additions { return additions(ctx, h, m) })
	return nil
}

// additions notes each of m's files, with the history's version, and shows
// the model what it can take of each.
func additions(ctx context.Context, h feature.Host, m feature.Message) feature.Additions {
	info := h.Info()
	var notes, kept []string
	var files []feature.File
	content := false // the model gets what a file says
	for i, md := range m.Media {
		if !md.File {
			continue
		}
		label, status, text, file := one(ctx, h, info, m, i)
		note, keep := label+status+"]", label+status+"]"
		if text != "" || file != nil {
			keep, content = label+status+notKept+"]", true
		}
		if text != "" {
			note += "\n" + txt.Fence(text, maxText)
		}
		notes, kept = append(notes, note), append(kept, keep)
		if file != nil {
			files = append(files, *file)
		}
	}
	if len(notes) == 0 {
		return feature.Additions{}
	}
	return feature.Additions{Note: strings.Join(notes, "\n"), HistoryNote: strings.Join(kept, "\n"), Files: files, Content: content}
}

// one handles m.Media[i]: its label (`[file "name"`), its status (such as
// " attached" or ", which failed to load"), and the text or file the model
// is shown, if any. It logs one line, what the file came to, by kind and
// size: never its name or declared type, which the sender chose.
func one(ctx context.Context, h feature.Host, info feature.Info, m feature.Message, i int) (label, status, text string, file *feature.File) {
	md := m.Media[i]
	name := cmp.Or(txt.Clamp(txt.SafeName(md.Name), maxName), "untitled")
	label = "[file " + strconv.Quote(name)
	if md.ID == "" {
		logx.Warnf("file no longer available")
		return label, ", which is no longer available", "", nil
	}
	data, err := h.Media(ctx, m, i)
	if err != nil {
		logx.Warnf("could not fetch a file: %v", err)
		return label, ", which failed to load", "", nil
	}
	ext, sniffed := strings.ToLower(path.Ext(md.Name)), http.DetectContentType(data)
	kind := kindOf(md.MIME, ext, sniffed, data)
	what := kind
	if kind == "office" {
		what = ext[1:] // docx, xlsx or pptx
	}
	head := fmt.Sprintf("file %s %d bytes", what, len(data))
	switch kind {
	case "pdf":
		if !info.Files {
			logx.Infof("%s, not sent: this model can't read files", head)
			return label, ", a PDF, which you can't read with this model", "", nil
		}
		logx.Infof("%s, attached", head)
		return label, " attached", "", &feature.File{Name: name, MIME: "application/pdf", Data: data}
	case "audio":
		if !info.Audio {
			logx.Infof("%s, not sent: this model can't hear audio", head)
			return label, ", audio such as a voice note, which you can't hear with this model", "", nil
		}
		mime := md.MIME
		if !strings.HasPrefix(strings.ToLower(mime), "audio/") {
			mime = cmp.Or(audioExts[ext], "audio/aac")
		}
		logx.Infof("%s, attached", head)
		return label, " attached", "", &feature.File{Name: name, MIME: mime, Data: data}
	case "office":
		if text, err = officeText(data, ext); err != nil || strings.TrimSpace(text) == "" {
			logx.Warnf("could not read an Office file (%s, %d bytes): %s", what, len(data), errKind(cmp.Or(err, errEmpty)))
			return label, ", which couldn't be read", "", nil
		}
		// The count is what the fence shows, without the newlines it trims.
		// officeText stops a little past the cap, so past it the document's
		// length is unknown.
		n, shown := utf8.RuneCountInString(strings.Trim(text, "\n")), ""
		if len(text) > maxText {
			shown = "first " + strconv.Itoa(min(n, maxText)) + " chars of text"
		} else {
			shown = txt.Chars(n, maxText)
		}
		logx.Infof("%s, %s", head, shown)
		return label, " attached, its text", text, nil
	case "text":
		logx.Infof("%s, %s", head, txt.Chars(utf8.RuneCount(bytes.Trim(data, "\n")), maxText))
		return label, " attached, its text", string(data), nil
	}
	logx.Infof("file %d bytes %s, not a kind the bot reads", len(data), txt.Clamp(sniffed, 40))
	return label, ", which you can't read", "", nil
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
