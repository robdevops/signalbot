package files

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// maxPart caps the bytes read from one part of an Office file and maxTotal
// those read from all its parts, so a small zip can't inflate into a huge
// one; maxDepth caps how deep elements nest, since the decoder keeps a
// stack of the open ones; maxParts caps a zip's entries, checked before its
// directory is built (a 1,000-slide deck has about 10,000). They are
// variables only so that this package's tests can lower them.
var (
	maxPart  = 10 << 20
	maxTotal = 32 << 20
	maxDepth = 256
	maxParts = 20_000
)

// The budgets' errors, which the log names by kind.
var (
	errParts = errors.New("too many parts")
	errDeep  = errors.New("nested too deep")
	errSize  = errors.New("too much xml")
)

var officeExts = map[string]bool{".docx": true, ".xlsx": true, ".pptx": true}

var (
	slidePart = regexp.MustCompile(`^ppt/slides/slide(\d+)\.xml$`)
	sheetPart = regexp.MustCompile(`^xl/worksheets/sheet(\d+)\.xml$`)
)

// officeText is the text of a docx (its paragraphs), xlsx (each sheet's
// rows, cells separated by tabs) or pptx (each slide's paragraphs), cut a
// little past maxText, or where the file's byte budget ran out: what was
// read by then stands, and cut says the document has more.
func officeText(data []byte, ext string) (text string, cut bool, err error) {
	if entries(data) > maxParts {
		return "", false, errParts
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", false, err
	}
	var out strings.Builder
	budget := maxTotal
	switch ext {
	case ".docx":
		err = paragraphs(z, "word/document.xml", "p", "t", &budget, &out)
	case ".pptx":
		for i, name := range numbered(z, slidePart) {
			if out.Len() > maxText {
				break
			}
			fmt.Fprintf(&out, "Slide %d:\n", i+1)
			if err = paragraphs(z, name, "p", "t", &budget, &out); err != nil {
				break
			}
		}
	case ".xlsx":
		err = sheets(z, &budget, &out)
	default:
		err = errors.New("not an Office file")
	}
	text = out.String()
	if errors.Is(err, errSize) && strings.TrimSpace(text) != "" {
		return text, true, nil
	}
	return text, len(text) > maxText, err
}

// errKind is what went wrong reading an Office file, for the log: the kind
// of error, never its text, since encoding/xml's messages quote the
// document (its declared encoding, element and entity names).
func errKind(err error) string {
	var syntax *xml.SyntaxError
	switch {
	case errors.Is(err, errEmpty):
		return "no text"
	case errors.Is(err, zip.ErrFormat), errors.Is(err, zip.ErrChecksum), errors.Is(err, zip.ErrAlgorithm):
		return "not a zip"
	case errors.Is(err, fs.ErrNotExist):
		return "missing part"
	case errors.As(err, &syntax):
		return "bad xml"
	case errors.Is(err, errParts):
		return "too many parts"
	case errors.Is(err, errDeep):
		return "too deep"
	case errors.Is(err, errSize):
		return "too much xml"
	}
	return "unreadable"
}

// entries is the number of files a zip's end record declares: the last
// end-of-central-directory signature in the final 65,557 bytes, the record
// archive/zip reads too, or -1 without one (archive/zip then says it isn't
// a zip). A zip64 count (0xffff) is over any cap, so a tiny file declaring
// a million entries is refused before its directory is built.
func entries(data []byte) int {
	tail := data[max(0, len(data)-(22+65535)):]
	i := bytes.LastIndex(tail, []byte("PK\x05\x06"))
	if i < 0 || i+12 > len(tail) {
		return -1
	}
	return int(binary.LittleEndian.Uint16(tail[i+10:]))
}

// part is the tokens of the zip's file called name, read to at most maxPart
// bytes (a longer part is cut there) and to what is left of the file's
// budget (errSize when it needs more), refusing elements nested past
// maxDepth (errDeep).
func part(z *zip.Reader, name string, budget *int) (next func() (xml.Token, error), err error) {
	f, err := z.Open(name)
	if err != nil {
		return nil, err
	}
	limit := min(maxPart, *budget)
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1)) // one more, to tell a cut from a fit
	f.Close()
	if err != nil {
		return nil, err
	}
	if len(data) > limit && limit == *budget {
		return nil, errSize
	}
	data = data[:min(len(data), limit)]
	*budget -= len(data)
	d, depth := xml.NewDecoder(bytes.NewReader(data)), 0
	return func() (xml.Token, error) {
		tok, err := d.Token()
		switch tok.(type) {
		case xml.StartElement:
			if depth++; depth > maxDepth {
				return nil, errDeep
			}
		case xml.EndElement:
			depth--
		}
		return tok, err
	}, nil
}

// numbered is the zip's files whose names match re, in the order of the
// number re captures.
func numbered(z *zip.Reader, re *regexp.Regexp) []string {
	type numberedName struct {
		n    int
		name string
	}
	var found []numberedName
	for _, f := range z.File {
		if m := re.FindStringSubmatch(f.Name); m != nil {
			n, _ := strconv.Atoi(m[1])
			found = append(found, numberedName{n, f.Name})
		}
	}
	slices.SortFunc(found, func(a, b numberedName) int { return a.n - b.n })
	var out []string
	for _, f := range found {
		out = append(out, f.name)
	}
	return out
}

// paragraphs writes the text of each element named para in the part, a line
// each, from the text elements named text in it, stopping past maxText.
func paragraphs(z *zip.Reader, name, para, text string, budget *int, out *strings.Builder) error {
	next, err := part(z, name, budget)
	if err != nil {
		return err
	}
	inText := false
	for out.Len() <= maxText {
		tok, err := next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			inText = t.Name.Local == text
			if t.Name.Local == "tab" {
				out.WriteByte('\t')
			}
		case xml.EndElement:
			inText = false
			if t.Name.Local == para {
				out.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				out.Write(t[:min(len(t), maxText+1-out.Len())])
			}
		}
	}
	return nil
}

// sheets writes each worksheet of an xlsx as "Sheet N:" and its rows, cells
// separated by tabs in their columns, from the shared strings and the
// cells' own values.
func sheets(z *zip.Reader, budget *int, out *strings.Builder) error {
	var shared []string
	if next, err := part(z, "xl/sharedStrings.xml", budget); err == nil {
		var cur strings.Builder
		inT := false
		for {
			tok, err := next()
			if err != nil {
				break
			}
			switch t := tok.(type) {
			case xml.StartElement:
				inT = t.Name.Local == "t"
				if t.Name.Local == "si" {
					cur.Reset()
				}
			case xml.EndElement:
				inT = false
				if t.Name.Local == "si" {
					shared = append(shared, cur.String())
				}
			case xml.CharData:
				if inT && cur.Len() <= maxText {
					cur.Write(t[:min(len(t), maxText+1-cur.Len())])
				}
			}
		}
	}
	for i, name := range numbered(z, sheetPart) {
		fmt.Fprintf(out, "Sheet %d:\n", i+1)
		if err := rows(z, name, shared, budget, out); err != nil {
			return err
		}
		if out.Len() > maxText {
			break
		}
	}
	return nil
}

// rows writes a worksheet's rows, a line each.
func rows(z *zip.Reader, name string, shared []string, budget *int, out *strings.Builder) error {
	next, err := part(z, name, budget)
	if err != nil {
		return err
	}
	var cellType, value string
	var row strings.Builder
	col, inValue := 0, false
	for out.Len() <= maxText {
		tok, err := next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				col = 0
				row.Reset()
			case "c":
				cellType, value = "", ""
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "t":
						cellType = a.Value
					case "r": // e.g. "C7": move to column C, so gaps stay gaps
						for c := column(a.Value); col < c && col < 100 && row.Len() <= maxText; col++ {
							row.WriteByte('\t')
						}
					}
				}
			case "v", "t":
				inValue = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v", "t":
				inValue = false
			case "c":
				if cellType == "s" {
					if n, err := strconv.Atoi(value); err == nil && n >= 0 && n < len(shared) {
						value = shared[n]
					}
				}
				if row.Len()+out.Len() <= maxText { // a row of many long shared strings stays small
					row.WriteString(strings.ReplaceAll(value, "\n", " "))
					row.WriteByte('\t')
				}
				col++
			case "row":
				out.WriteString(strings.TrimRight(row.String(), "\t") + "\n")
			}
		case xml.CharData:
			if inValue && len(value) <= maxText {
				value += string(t[:min(len(t), maxText+1-len(value))])
			}
		}
	}
	return nil
}

// column is the 0-based column of a cell reference such as "C7" (2).
func column(ref string) int {
	n := 0
	for _, r := range ref {
		if r < 'A' || r > 'Z' {
			break
		}
		n = n*26 + int(r-'A'+1)
	}
	return n - 1
}
