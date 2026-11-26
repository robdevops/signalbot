package addon

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// schema is schema.json, decoded.
func schema(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// defs is the schema's $defs.
func defs(t *testing.T, s map[string]any) map[string]any {
	t.Helper()
	d, ok := s["$defs"].(map[string]any)
	if !ok {
		t.Fatal("schema.json has no $defs")
	}
	return d
}

// wireTypes are the Go types schema.json describes, by their $defs name.
var wireTypes = map[string]reflect.Type{
	"Capability": reflect.TypeFor[Capability](),
	"Context":    reflect.TypeFor[Context](),
	"Watch":      reflect.TypeFor[Watch](),
	"Content":    reflect.TypeFor[Content](),
	"Result":     reflect.TypeFor[Result](),
	"ResultMeta": reflect.TypeFor[ResultMeta](),
	"Op":         reflect.TypeFor[Op](),
	"Image":      reflect.TypeFor[Image](),
	"Audio":      reflect.TypeFor[Audio](),
	"Button":     reflect.TypeFor[Button](),
	"Press":      reflect.TypeFor[Press](),
	"Page":       reflect.TypeFor[Page](),
	"Held":       reflect.TypeFor[Held](),

	"SampleRequest": reflect.TypeFor[SampleRequest](),
	"SampleMeta":    reflect.TypeFor[SampleMeta](),
	"SampleMessage": reflect.TypeFor[SampleMessage](),
	"SampleResult":  reflect.TypeFor[SampleResult](),
}

// The schema and the Go types say the same: the same objects, each with
// the same keys, the same keys required (those Go always writes), and the
// same type for each.
func TestSchemaMatchesTypes(t *testing.T) {
	d := defs(t, schema(t))
	for name := range d {
		if _, ok := wireTypes[name]; !ok && name != "ToolMeta" {
			t.Errorf("$defs/%s has no Go type", name)
		}
	}
	// Every struct types.go declares is in wireTypes, so in the schema.
	f, err := parser.ParseFile(token.NewFileSet(), "types.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		g, ok := decl.(*ast.GenDecl)
		if !ok || g.Tok != token.TYPE {
			continue
		}
		for _, spec := range g.Specs {
			ts := spec.(*ast.TypeSpec)
			if _, isStruct := ts.Type.(*ast.StructType); isStruct && ts.Name.IsExported() && wireTypes[ts.Name.Name] == nil {
				t.Errorf("types.go's %s isn't in wireTypes, so the schema isn't checked for it", ts.Name.Name)
			}
		}
	}
	for name, typ := range wireTypes {
		def, ok := d[name].(map[string]any)
		if !ok {
			t.Errorf("%s isn't in $defs", typ)
			continue
		}
		props, _ := def["properties"].(map[string]any)
		var keys, required []string
		for i := range typ.NumField() {
			f := typ.Field(i)
			tag, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			if !f.IsExported() || tag == "-" || tag == "" {
				t.Errorf("%s.%s has no JSON name", typ, f.Name)
				continue
			}
			keys = append(keys, tag)
			if !slices.Contains(strings.Split(opts, ","), "omitempty") {
				required = append(required, tag)
			}
			p, ok := props[tag].(map[string]any)
			if !ok {
				t.Errorf("$defs/%s has no %q, %s.%s's key", name, tag, typ, f.Name)
				continue
			}
			// A Go string may hold a formatted one, such as Watch.Until.
			if want, got := goSchema(f.Type), propType(p); want != got && (want != "string" || !strings.HasPrefix(got, "string(")) {
				t.Errorf("$defs/%s %q is %s; %s.%s is %s", name, tag, got, typ, f.Name, want)
			}
		}
		var schemaKeys []string
		for k := range props {
			schemaKeys = append(schemaKeys, k)
		}
		var schemaRequired []string
		for _, r := range asList(def["required"]) {
			schemaRequired = append(schemaRequired, r.(string))
		}
		sort.Strings(keys)
		sort.Strings(schemaKeys)
		sort.Strings(required)
		sort.Strings(schemaRequired)
		if !slices.Equal(keys, schemaKeys) {
			t.Errorf("$defs/%s has keys %v; %s has %v", name, schemaKeys, typ, keys)
		}
		if !slices.Equal(required, schemaRequired) {
			t.Errorf("$defs/%s requires %v; %s always writes %v", name, schemaRequired, typ, required)
		}
	}
	// The _meta keys are the package's.
	for key, def := range map[string]string{KeyVisibility: "ToolMeta", KeyWatch: "ResultMeta", KeyReact: "ResultMeta", KeyButtons: "ResultMeta", KeyOnPress: "ResultMeta", KeyChat: "SampleMeta"} {
		if _, ok := d[def].(map[string]any)["properties"].(map[string]any)[key]; !ok {
			t.Errorf("$defs/%s lacks %s", def, key)
		}
	}
	// A pseudonym's pattern is the same wherever it appears.
	prop := func(def, key string) any {
		return d[def].(map[string]any)["properties"].(map[string]any)[key].(map[string]any)["pattern"]
	}
	for _, k := range []string{"chat", "ref"} {
		if prop("Op", k) != prop("Context", k) {
			t.Errorf("Op's %s pattern %v isn't Context's %v", k, prop("Op", k), prop("Context", k))
		}
	}
	if prop("SampleMeta", KeyChat) != prop("Context", "chat") {
		t.Errorf("SampleMeta's chat pattern %v isn't Context's %v", prop("SampleMeta", KeyChat), prop("Context", "chat"))
	}
	if v := d["Capability"].(map[string]any)["properties"].(map[string]any)["version"].(map[string]any)["const"]; v != float64(Version) {
		t.Errorf("the schema's version is %v; the package's %d", v, Version)
	}
	if n := d["Op"].(map[string]any)["properties"].(map[string]any)["images"].(map[string]any)["maxItems"]; n != float64(MaxImages) {
		t.Errorf("the schema's most images is %v; the package's %d", n, MaxImages)
	}
	if title := schema(t)["title"].(string); !strings.Contains(title, Protocol) {
		t.Errorf("the title %q doesn't name MCP %s", title, Protocol)
	}
}

// goSchema is the schema type a Go type is written as: a JSON type, a
// $ref to another of $defs, or an array of either.
func goSchema(t reflect.Type) string {
	if t == reflect.TypeFor[time.Time]() {
		return "string(date-time)"
	}
	switch t.Kind() {
	case reflect.Pointer:
		return goSchema(t.Elem())
	case reflect.Struct:
		return "#/$defs/" + t.Name()
	case reflect.Slice:
		return "array of " + goSchema(t.Elem())
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int64:
		return "integer"
	case reflect.Float64:
		return "number"
	}
	return "unknown " + t.String()
}

// propType is a property's type as goSchema writes it. A const stands for
// its value's type.
func propType(p map[string]any) string {
	if ref, ok := p["$ref"].(string); ok {
		return ref
	}
	typ, _ := p["type"].(string)
	switch {
	case typ == "array":
		items, _ := p["items"].(map[string]any)
		return "array of " + propType(items)
	case typ == "string" && p["format"] == "date-time":
		return "string(date-time)"
	case typ == "":
		if _, ok := p["const"].(float64); ok {
			return "integer"
		}
		if e := asList(p["enum"]); len(e) > 0 {
			if _, ok := e[0].(string); ok {
				return "string"
			}
		}
	}
	return typ
}

func asList(v any) []any { l, _ := v.([]any); return l }

// validate reports why v breaks the schema s (a subschema of root), or nil.
// It knows the keywords schema.json uses, and fails on any other, so a
// keyword added to the schema can't be skipped here unnoticed.
func validate(root, s map[string]any, v any, at string) error {
	for k, kv := range s {
		switch k {
		case "$schema", "$id", "title", "description", "examples", "format", "contentEncoding", "$defs", "properties", "additionalProperties":
		case "$ref":
			name, ok := strings.CutPrefix(kv.(string), "#/$defs/")
			def, found := root["$defs"].(map[string]any)[name].(map[string]any)
			if !ok || !found {
				return fmt.Errorf("%s: no %s", at, kv)
			}
			if err := validate(root, def, v, at); err != nil {
				return err
			}
		case "type":
			if !isType(v, kv.(string)) {
				return fmt.Errorf("%s: not %s", at, kv)
			}
		case "const":
			if !reflect.DeepEqual(v, kv) {
				return fmt.Errorf("%s: not %v", at, kv)
			}
		case "enum":
			if !slices.ContainsFunc(asList(kv), func(e any) bool { return reflect.DeepEqual(v, e) }) {
				return fmt.Errorf("%s: %v isn't one of %v", at, v, kv)
			}
		case "pattern":
			if str, ok := v.(string); ok && !regexp.MustCompile(kv.(string)).MatchString(str) {
				return fmt.Errorf("%s: %q doesn't match %s", at, str, kv)
			}
		case "minLength", "maxLength":
			if str, ok := v.(string); ok {
				n := float64(len([]rune(str)))
				if k == "minLength" && n < kv.(float64) || k == "maxLength" && n > kv.(float64) {
					return fmt.Errorf("%s: %d characters, %s %v", at, len([]rune(str)), k, kv)
				}
			}
		case "minimum":
			if n, ok := v.(float64); ok && n < kv.(float64) {
				return fmt.Errorf("%s: %v under %v", at, n, kv)
			}
		case "minItems":
			if l, ok := v.([]any); ok && float64(len(l)) < kv.(float64) {
				return fmt.Errorf("%s: %d items, at least %v", at, len(l), kv)
			}
		case "maxItems":
			if l, ok := v.([]any); ok && float64(len(l)) > kv.(float64) {
				return fmt.Errorf("%s: %d items, at most %v", at, len(l), kv)
			}
		case "uniqueItems":
			l, _ := v.([]any)
			for i := range l {
				for j := range i {
					if reflect.DeepEqual(l[i], l[j]) {
						return fmt.Errorf("%s: %v twice", at, l[i])
					}
				}
			}
		case "items":
			for i, e := range asList(v) {
				if err := validate(root, kv.(map[string]any), e, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		case "required":
			if o, ok := v.(map[string]any); ok {
				for _, r := range asList(kv) {
					if _, ok := o[r.(string)]; !ok {
						return fmt.Errorf("%s: no %s", at, r)
					}
				}
			}
		case "allOf":
			for _, sub := range asList(kv) {
				if err := validate(root, sub.(map[string]any), v, at); err != nil {
					return err
				}
			}
		case "anyOf":
			var errs []string
			for _, sub := range asList(kv) {
				err := validate(root, sub.(map[string]any), v, at)
				if err == nil {
					errs = nil
					break
				}
				errs = append(errs, err.Error())
			}
			if errs != nil {
				return fmt.Errorf("%s: none of anyOf: %s", at, strings.Join(errs, "; "))
			}
		case "if":
			branch := "then"
			if validate(root, kv.(map[string]any), v, at) != nil {
				branch = "else"
			}
			if sub, ok := s[branch].(map[string]any); ok {
				if err := validate(root, sub, v, at); err != nil {
					return err
				}
			}
		case "not":
			if validate(root, kv.(map[string]any), v, at) == nil {
				return fmt.Errorf("%s: matches not %v", at, kv)
			}
		case "then", "else":
		default:
			return fmt.Errorf("%s: the test's validator doesn't know %q", at, k)
		}
	}
	o, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	props, _ := s["properties"].(map[string]any)
	for key, val := range o {
		p, known := props[key].(map[string]any)
		switch {
		case known:
			if err := validate(root, p, val, at+"."+key); err != nil {
				return err
			}
		case s["additionalProperties"] == false:
			return fmt.Errorf("%s: unknown key %q", at, key)
		}
	}
	return nil
}

// isType reports whether a decoded JSON value is of a JSON Schema type.
func isType(v any, typ string) bool {
	switch typ {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		n, ok := v.(float64)
		return ok && n == float64(int64(n))
	case "number":
		_, ok := v.(float64)
		return ok
	}
	return false
}

// check validates v, as JSON, against $defs/def.
func check(t *testing.T, root map[string]any, def string, v any) error {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	return validate(root, map[string]any{"$ref": "#/$defs/" + def}, decoded, def)
}

// Pseudonyms as the bot makes them.
const (
	chatID   = "c_abcdefghijklmnopqrstuvwxyz"
	senderID = "u_abcdefghijklmnopqrstuvwx23"
	refID    = "r_234567abcdefghijklmnopqrst"
)

// Messages as the bot and the package write them are valid, and the
// mistakes the schema is for are not.
func TestSchemaSamples(t *testing.T) {
	root := schema(t)
	until := time.Date(2026, 11, 21, 9, 3, 0, 0, time.UTC).Format(time.RFC3339)
	post := Op{ID: "a1b2.1", Op: "post", Chat: chatID, Text: "{" + senderID + "} wins"}
	temp := 0.7
	sample := SampleRequest{Messages: []SampleMessage{{Role: "user", Content: Content{Type: "text", Text: "a question"}}}, MaxTokens: 100, Meta: SampleMeta{Chat: chatID}}
	withSample := func(f func(m map[string]any)) map[string]any {
		data, _ := json.Marshal(sample)
		var m map[string]any
		json.Unmarshal(data, &m)
		f(m)
		return m
	}
	valid := []struct {
		def string
		v   any
	}{
		{"Capability", Capability{Version: Version, Network: "signal"}},
		{"Capability", Capability{Version: Version}},
		{"Context", Context{Chat: chatID, Sender: senderID, Ref: refID, Addressed: true, Group: true, Sent: 1763715600}},
		{"Context", Context{Chat: chatID, Sender: senderID, Ref: refID, Sent: 1763715600, Edited: 1763715660, Name: "Alice", Deadline: 1763715615}},
		{"Watch", Watch{ID: "r1", Pattern: `maps\.google`, Until: until, Tool: "guess", Guard: "No hints 😉"}},
		{"Watch", Watch{ID: strings.Repeat("é", 64), Pattern: `\bdevinette \d+`, Until: until, Tool: "guess"}},
		{"Result", Text("started")},
		{"Result", Text("")}, // MCP's TextContent needs its text, even empty
		{"Op", Op{ID: "5", Op: "post", Chat: chatID, Text: "", Images: []Image{{Data: "aGk=", MimeType: "image/png"}}}},
		{"Result", Result{Content: []Content{{Type: "image", Data: "aGk=", MimeType: "image/jpeg"}, {Type: "text", Text: "go"}},
			Meta: &ResultMeta{Watch: &Watch{ID: "r1", Pattern: `\d`, Until: until, Tool: "guess"}}}},
		{"Result", Result{Content: []Content{{Type: "text", Text: "no"}}, IsError: true, Meta: &ResultMeta{React: "📍"}}},
		{"Op", post},
		{"Op", Op{ID: "2", Op: "post", Chat: chatID, Images: []Image{{Data: "aGk=", MimeType: "image/png", Name: "a.png"}}}},
		{"Op", Op{ID: "6", Op: "post", Chat: chatID, Audio: &Audio{Data: "SUQz", MimeType: "audio/mpeg"}}},
		{"Op", Op{ID: "3", Op: "react", Ref: refID, Emoji: "🎯"}},
		{"Op", Op{ID: "4", Op: "unwatch", Chat: chatID, Watch: "r1"}},
		{"Op", Op{ID: "8", Op: "buttons", Chat: chatID, Watch: "lobby1", Buttons: [][]Button{{{Label: "▶️ Start", Data: "start:g1"}, {Label: "✖️ Cancel", Data: "cancel:g1"}}}}},
		{"Op", Op{ID: "7", Op: "react", Chat: chatID, Post: "a1b2.1", Emoji: "⏰"}},
		{"Op", Op{ID: "6", Op: "post", Chat: chatID, Text: "It was Paris.", ReplyTo: "a1b2.1"}},
		{"Result", Result{Content: []Content{{Type: "text", Text: "Starts in 60s"}}, Meta: &ResultMeta{
			Buttons: [][]Button{{{Label: "▶️ Start", Data: "start:g1"}, {Label: strings.Repeat("é", 32), Data: strings.Repeat("x", 64)}}}, OnPress: "geo_press"}}},
		{"Capability", Capability{Version: Version, Network: "telegram", Buttons: true}},
		{"Press", Press{Press: "start:g1"}},
		{"Page", Page{Cursor: "a1b2.4", Ops: []Op{post}}},
		{"Page", Page{Cursor: "a1b2.0", More: true, Ops: []Op{}}},
		{"Held", Held{Op: post, Queued: time.Now()}},
		{"ToolMeta", map[string]any{KeyVisibility: []string{"bot"}}},
		{"ToolMeta", map[string]any{}},
		{"Capability", Capability{Version: Version, Network: "signal", WatchReplies: true}},
		{"Watch", Watch{ID: "q1", Until: until, Tool: "answer", Replies: true}},
		{"Watch", Watch{ID: "q1", Pattern: `(?i)^\s*[a-d]\s*$`, Until: until, Tool: "answer", Replies: true}},
		{"SampleRequest", sample},
		{"SampleRequest", SampleRequest{Messages: []SampleMessage{{Role: "user", Content: Content{Type: "text", Text: "a question"}},
			{Role: "assistant", Content: Content{Type: "text", Text: "{"}}}, SystemPrompt: "Answer in JSON.", MaxTokens: 2000, Temperature: &temp,
			Meta: SampleMeta{Chat: chatID}}},
		{"SampleResult", SampleResult{Role: "assistant", Content: Content{Type: "text", Text: "an answer"}, Model: "a-model", StopReason: "endTurn"}},
		{"SampleResult", SampleResult{Role: "assistant", Content: Content{Type: "text"}, Model: "a-model"}},
	}
	for _, c := range valid {
		if err := check(t, root, c.def, c.v); err != nil {
			t.Errorf("%+v: %v", c.v, err)
		}
	}
	invalid := []struct {
		def, why string
		v        any
	}{
		{"Capability", "another version", map[string]any{"version": 2}},
		{"Context", "a phone number for a sender", Context{Chat: chatID, Sender: "+61400000000", Ref: refID}},
		{"Context", "no ref", map[string]any{"chat": chatID, "sender": senderID, "addressed": true, "group": true, "sent": 1, "edited": 0}},
		{"Watch", "no tool", Watch{ID: "r1", Pattern: `\d`, Until: until}},
		{"Watch", "a misspelt key", map[string]any{"id": "r1", "pattern": `\d`, "until": until, "tool": "guess", "gaurd": "x"}},
		{"Watch", "too long an ID", Watch{ID: strings.Repeat("x", 65), Pattern: `\d`, Until: until, Tool: "guess"}},
		{"Watch", "too long an ID, accented", Watch{ID: strings.Repeat("é", 65), Pattern: `\d`, Until: until, Tool: "guess"}},
		{"Result", "an image without its bytes", Result{Content: []Content{{Type: "image", MimeType: "image/jpeg"}}}},
		{"Result", "a text block without its text", map[string]any{"content": []any{map[string]any{"type": "text"}}}},
		{"Result", "audio", Result{Content: []Content{{Type: "audio", Data: "aGk=", MimeType: "audio/wav"}}}},
		{"Result", "a watch without a pattern", Result{Content: []Content{}, Meta: &ResultMeta{Watch: &Watch{ID: "r1", Until: until, Tool: "g"}}}},
		{"Op", "no ID", Op{Op: "unwatch", Chat: chatID, Watch: "r1"}},
		{"Op", "an unknown op", Op{ID: "1", Op: "delete", Chat: chatID}},
		{"Op", "a post without a chat", Op{ID: "1", Op: "post", Text: "hi"}},
		{"Op", "an empty post", Op{ID: "1", Op: "post", Chat: chatID}},
		{"Op", "a post of empty text", map[string]any{"id": "1", "op": "post", "chat": chatID, "text": " \n"}},
		{"Op", "a post of no images", map[string]any{"id": "1", "op": "post", "chat": chatID, "images": []any{}}},
		{"Op", "three images", Op{ID: "1", Op: "post", Chat: chatID, Images: slices.Repeat([]Image{{Data: "aGk=", MimeType: "image/png"}}, MaxImages+1)}},
		{"Op", "an image without its type", map[string]any{"id": "1", "op": "post", "chat": chatID, "images": []any{map[string]any{"data": "aGk="}}}},
		{"Op", "a reaction without an emoji", Op{ID: "1", Op: "react", Ref: refID}},
		{"Op", "audio that isn't MP3", Op{ID: "1", Op: "post", Chat: chatID, Audio: &Audio{Data: "T2dn", MimeType: "audio/ogg"}}},
		{"Op", "a reaction to a chat", Op{ID: "1", Op: "react", Ref: chatID, Emoji: "🎯"}},
		{"Op", "an unwatch without its watch", Op{ID: "1", Op: "unwatch", Chat: chatID}},
		{"Op", "a buttons op without buttons", Op{ID: "1", Op: "buttons", Chat: chatID, Watch: "lobby1"}},
		{"Op", "a buttons op without its watch", Op{ID: "1", Op: "buttons", Chat: chatID, Buttons: [][]Button{{{Label: "a", Data: "a"}}}}},
		{"Op", "a buttons op without its chat", Op{ID: "1", Op: "buttons", Watch: "lobby1", Buttons: [][]Button{{{Label: "a", Data: "a"}}}}},
		{"Op", "a buttons op with six rows", Op{ID: "1", Op: "buttons", Chat: chatID, Watch: "lobby1", Buttons: slices.Repeat([][]Button{{{Label: "a", Data: "a"}}}, 6)}},
		{"Op", "a buttons op with a long label", Op{ID: "1", Op: "buttons", Chat: chatID, Watch: "lobby1", Buttons: [][]Button{{{Label: strings.Repeat("x", 33), Data: "a"}}}}},
		{"Op", "a post with buttons", Op{ID: "1", Op: "post", Chat: chatID, Text: "hi", Buttons: [][]Button{{{Label: "a", Data: "a"}}}}},
		{"Op", "a group ID for a chat", Op{ID: "1", Op: "unwatch", Chat: "group.abc=", Watch: "r1"}},
		{"Op", "an empty replyTo", map[string]any{"id": "1", "op": "post", "chat": chatID, "text": "hi", "replyTo": ""}},
		{"Op", "a replyTo that isn't a string", map[string]any{"id": "1", "op": "post", "chat": chatID, "text": "hi", "replyTo": 1}},
		{"Op", "a reaction with a replyTo", Op{ID: "1", Op: "react", Ref: refID, Emoji: "🎯", ReplyTo: "a1b2.1"}},
		{"Op", "an unwatch with a replyTo", Op{ID: "1", Op: "unwatch", Chat: chatID, Watch: "r1", ReplyTo: "a1b2.1"}},
		{"Op", "a reaction with neither ref nor post", Op{ID: "1", Op: "react", Chat: chatID, Emoji: "⏰"}},
		{"Op", "a reaction with both ref and post", Op{ID: "1", Op: "react", Ref: refID, Chat: chatID, Post: "a1b2.1", Emoji: "⏰"}},
		{"Op", "a reaction to a post without its chat", Op{ID: "1", Op: "react", Post: "a1b2.1", Emoji: "⏰"}},
		{"Op", "a reaction to a post without an emoji", Op{ID: "1", Op: "react", Chat: chatID, Post: "a1b2.1"}},
		{"Op", "a reaction to an empty post ID", map[string]any{"id": "1", "op": "react", "chat": chatID, "post": "", "emoji": "⏰"}},
		{"Op", "a post op with a post", Op{ID: "1", Op: "post", Chat: chatID, Text: "hi", Post: "a1b2.1"}},
		{"Result", "buttons without onPress", Result{Content: []Content{}, Meta: &ResultMeta{Buttons: [][]Button{{{Label: "a", Data: "a"}}}}}},
		{"Result", "onPress without buttons", Result{Content: []Content{}, Meta: &ResultMeta{OnPress: "p"}}},
		{"Result", "six rows of buttons", Result{Content: []Content{}, Meta: &ResultMeta{Buttons: slices.Repeat([][]Button{{{Label: "a", Data: "a"}}}, 6), OnPress: "p"}}},
		{"Result", "six buttons in a row", Result{Content: []Content{}, Meta: &ResultMeta{Buttons: [][]Button{slices.Repeat([]Button{{Label: "a", Data: "a"}}, 6)}, OnPress: "p"}}},
		{"Result", "an empty row", map[string]any{"content": []any{}, "_meta": map[string]any{KeyButtons: []any{[]any{}}, KeyOnPress: "p"}}},
		{"Result", "a long label", Result{Content: []Content{}, Meta: &ResultMeta{Buttons: [][]Button{{{Label: strings.Repeat("x", 33), Data: "a"}}}, OnPress: "p"}}},
		{"Result", "a label with a space after it", Result{Content: []Content{}, Meta: &ResultMeta{Buttons: [][]Button{{{Label: "Start ", Data: "a"}}}, OnPress: "p"}}},
		{"Result", "long data", Result{Content: []Content{}, Meta: &ResultMeta{Buttons: [][]Button{{{Label: "a", Data: strings.Repeat("x", 65)}}}, OnPress: "p"}}},
		{"Result", "empty data", map[string]any{"content": []any{}, "_meta": map[string]any{KeyButtons: []any{[]any{map[string]any{"label": "a", "data": ""}}}, KeyOnPress: "p"}}},
		{"Capability", "buttons not a boolean", map[string]any{"version": 1, "buttons": "yes"}},
		{"Press", "no press", map[string]any{}},
		{"Page", "no cursor", map[string]any{"more": false, "ops": []any{}}},
		{"Page", "a bad op", Page{Cursor: "x.1", Ops: []Op{{ID: "1", Op: "post"}}}},
		{"Held", "no time", map[string]any{"op": post}},
		{"ToolMeta", "an unknown caller", map[string]any{KeyVisibility: []string{"everyone"}}},
		{"Capability", "watchReplies not a boolean", map[string]any{"version": 1, "watchReplies": "yes"}},
		{"Watch", "no pattern and no replies", Watch{ID: "r1", Until: until, Tool: "guess"}},
		{"Watch", "no pattern, replies false", map[string]any{"id": "r1", "until": until, "tool": "guess", "replies": false}},
		{"Watch", "an empty pattern with replies", map[string]any{"id": "r1", "pattern": "", "until": until, "tool": "guess", "replies": true}},
		{"Watch", "replies not a boolean", map[string]any{"id": "r1", "until": until, "tool": "guess", "replies": "yes"}},
		{"SampleRequest", "no chat", withSample(func(m map[string]any) { m["_meta"] = map[string]any{} })},
		{"SampleRequest", "no _meta", withSample(func(m map[string]any) { delete(m, "_meta") })},
		{"SampleRequest", "a sender for a chat", withSample(func(m map[string]any) { m["_meta"] = map[string]any{KeyChat: senderID} })},
		{"SampleRequest", "no maxTokens", withSample(func(m map[string]any) { delete(m, "maxTokens") })},
		{"SampleRequest", "no tokens", withSample(func(m map[string]any) { m["maxTokens"] = 0 })},
		{"SampleRequest", "no messages", withSample(func(m map[string]any) { m["messages"] = []any{} })},
		{"SampleRequest", "17 messages", withSample(func(m map[string]any) { m["messages"] = slices.Repeat(m["messages"].([]any), 17) })},
		{"SampleRequest", "a temperature in words", withSample(func(m map[string]any) { m["temperature"] = "warm" })},
		{"SampleRequest", "a system role", withSample(func(m map[string]any) {
			m["messages"] = []any{map[string]any{"role": "system", "content": map[string]any{"type": "text", "text": "x"}}}
		})},
		{"SampleRequest", "an image", withSample(func(m map[string]any) {
			m["messages"] = []any{map[string]any{"role": "user", "content": map[string]any{"type": "image", "data": "aGk=", "mimeType": "image/png"}}}
		})},
		{"SampleResult", "no model", map[string]any{"role": "assistant", "content": map[string]any{"type": "text", "text": "x"}}},
		{"SampleResult", "the user's", SampleResult{Role: "user", Content: Content{Type: "text", Text: "x"}, Model: "m"}},
		{"SampleResult", "a text block without its text", map[string]any{"role": "assistant", "content": map[string]any{"type": "text"}, "model": "m"}},
	}
	for _, c := range invalid {
		if check(t, root, c.def, c.v) == nil {
			t.Errorf("%s (%s) passed", c.def, c.why)
		}
	}
}

// What a Server really writes is valid: its tools' _meta, a tool's result
// with a watch, and an outbox page.
func TestSchemaServer(t *testing.T) {
	root := schema(t)
	until := time.Now().Add(time.Minute).Format(time.RFC3339)
	var s *Server
	s = New(Config{Name: "game", Tools: []Tool{{
		Name: "start", Visibility: []string{"bot"},
		Call: func(_ context.Context, c *Context, _ json.RawMessage) (Result, error) {
			if err := s.Queue(Op{Op: "post", Chat: c.Chat, Text: "{" + c.Sender + "} started"},
				Op{Op: "react", Ref: c.Ref, Emoji: "🎲"}, Op{Op: "unwatch", Chat: c.Chat, Watch: "r1"}); err != nil {
				return Result{}, err
			}
			r := Text("started")
			r.Meta = &ResultMeta{Watch: &Watch{ID: "r1", Pattern: `\bguess \d+`, Until: until, Tool: "start"}}
			return r, nil
		},
	}}})
	c := start(t, s)
	var init struct {
		Capabilities struct {
			Experimental map[string]any `json:"experimental"`
		} `json:"capabilities"`
	}
	c.result("initialize", map[string]any{"protocolVersion": Protocol, "capabilities": map[string]any{
		"experimental": map[string]any{KeyAddon: Capability{Version: Version, Network: "telegram"}}}}, &init)
	if err := validate(root, map[string]any{"$ref": "#/$defs/Capability"}, init.Capabilities.Experimental[KeyAddon], "Capability"); err != nil {
		t.Error(err)
	}
	var list struct {
		Tools []struct {
			Meta any `json:"_meta"`
		} `json:"tools"`
	}
	c.result("tools/list", struct{}{}, &list)
	for _, tl := range list.Tools {
		if err := validate(root, map[string]any{"$ref": "#/$defs/ToolMeta"}, tl.Meta, "ToolMeta"); err != nil {
			t.Error(err)
		}
	}
	var res any
	c.result("tools/call", map[string]any{"name": "start", "arguments": map[string]any{},
		"_meta": map[string]any{KeyContext: Context{Chat: chatID, Sender: senderID, Ref: refID, Sent: 1}}}, &res)
	if err := validate(root, map[string]any{"$ref": "#/$defs/Result"}, res, "Result"); err != nil {
		t.Error(err)
	}
	var read struct {
		Contents []resourceContent
	}
	c.result("resources/read", uriParams{OutboxURI + "/0"}, &read)
	var page any
	if len(read.Contents) != 1 || json.Unmarshal([]byte(read.Contents[0].Text), &page) != nil {
		t.Fatalf("read %+v", read)
	}
	if err := validate(root, map[string]any{"$ref": "#/$defs/Page"}, page, "Page"); err != nil {
		t.Error(err)
	}
	if ops := page.(map[string]any)["ops"].([]any); len(ops) != 3 {
		t.Errorf("ops %v", ops)
	}
	for _, h := range s.Outbox() {
		if err := check(t, root, "Held", h); err != nil {
			t.Error(err)
		}
	}
}
