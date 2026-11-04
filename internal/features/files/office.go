package files

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// maxPart caps the bytes read from one part of an Office file, so a small
// zip can't inflate into a huge one.
const maxPart = 10 << 20

var officeExts = map[string]bool{".docx": true, ".xlsx": true, ".pptx": true}

var (
	slidePart = regexp.MustCompile(`^ppt/slides/slide(\d+)\.xml$`)
	sheetPart = regexp.MustCompile(`^xl/worksheets/sheet(\d+)\.xml$`)
)

// officeText is the text of a docx (its paragraphs), xlsx (each sheet's
// rows, cells separated by tabs) or pptx (each slide's paragraphs), cut a
// little past maxText.
func officeText(data []byte, ext string) (string, error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	var out strings.Builder
	switch ext {
	case ".docx":
		err = paragraphs(z, "word/document.xml", "p", "t", &out)
	case ".pptx":
		for i, name := range numbered(z, slidePart) {
			if out.Len() > maxText {
				break
			}
			fmt.Fprintf(&out, "Slide %d:\n", i+1)
			if err = paragraphs(z, name, "p", "t", &out); err != nil {
				break
			}
		}
	case ".xlsx":
		err = sheets(z, &out)
	default:
		err = errors.New("not an Office file")
	}
	return out.String(), err
}

// part opens the zip's file called name, reading at most maxPart bytes.
func part(z *zip.Reader, name string) (*xml.Decoder, error) {
	f, err := z.Open(name)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPart))
	f.Close()
	if err != nil {
		return nil, err
	}
	return xml.NewDecoder(bytes.NewReader(data)), nil
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
func paragraphs(z *zip.Reader, name, para, text string, out *strings.Builder) error {
	d, err := part(z, name)
	if err != nil {
		return err
	}
	inText := false
	for out.Len() <= maxText {
		tok, err := d.Token()
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
func sheets(z *zip.Reader, out *strings.Builder) error {
	var shared []string
	if d, err := part(z, "xl/sharedStrings.xml"); err == nil {
		var cur strings.Builder
		inT := false
		for {
			tok, err := d.Token()
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
		if err := rows(z, name, shared, out); err != nil {
			return err
		}
		if out.Len() > maxText {
			break
		}
	}
	return nil
}

// rows writes a worksheet's rows, a line each.
func rows(z *zip.Reader, name string, shared []string, out *strings.Builder) error {
	d, err := part(z, name)
	if err != nil {
		return err
	}
	var cellType, value string
	var row strings.Builder
	col, inValue := 0, false
	for out.Len() <= maxText {
		tok, err := d.Token()
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
