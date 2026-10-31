// Command emoji-gen writes the emoji catalog from the vendored Unicode data
// (issue #953, docs/design/message-reactions.md §3.1): from one run over
// third_party/unicode/emoji-test.txt it writes pkg/schema/emoji_catalog_gen.go,
// the table CanonicalReactionKey validates against and schema.EmojiVersion,
// and web/src/emoji/catalog.json, the browser picker's catalog, which carries
// the same version string. `go generate ./pkg/schema` runs it;
// cmd/emoji-gen's own test regenerates both into memory and fails when the
// committed files differ.
//
// It refuses to run when the data file's SHA-256 is not the pinned one
// (third_party/unicode/emoji-test.sha256), so a hand edit to the data never
// reaches the catalog: scripts/emoji-update.sh is how the pin moves. It also
// fails, writing nothing, when two fully-qualified sequences collide once
// their presentation selectors (U+FE0F, U+FE0E) are stripped, since the lookup
// table would then be ambiguous, or when a listed minimally-qualified or
// unqualified form strips to no fully-qualified key.
//
// It reads the file by its own simple grammar and imports nothing from
// pkg/schema, which it generates part of.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

func main() {
	data := flag.String("data", "", "the vendored emoji-test.txt")
	pin := flag.String("sha256", "", "the file holding its pinned SHA-256 (sha256sum form)")
	goOut := flag.String("go", "", "the Go file to write (package schema)")
	jsonOut := flag.String("json", "", "the JSON catalog to write")
	aliases := flag.String("aliases", "", "Conch's shortcode alias table (aliases.txt)")
	flag.Parse()
	if *data == "" || *pin == "" || *aliases == "" || *goOut == "" || *jsonOut == "" || flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: emoji-gen -data <emoji-test.txt> -sha256 <pin file> -aliases <aliases.txt> -go <out.go> -json <out.json>")
		os.Exit(2)
	}
	if err := run(*data, *pin, *aliases, *goOut, *jsonOut); err != nil {
		fmt.Fprintln(os.Stderr, "emoji-gen:", err)
		os.Exit(1)
	}
}

// run generates both outputs into memory first, so that a refusal writes
// nothing, then writes them.
func run(dataPath, pinPath, aliasPath, goPath, jsonPath string) error {
	data, err := os.ReadFile(dataPath) // #nosec G304 -- the path is this program's own flag
	if err != nil {
		return err
	}
	pin, err := os.ReadFile(pinPath) // #nosec G304 -- the path is this program's own flag
	if err != nil {
		return err
	}
	if err := verifyPin(data, pin, dataPath); err != nil {
		return err
	}
	c, err := parse(data)
	if err != nil {
		return err
	}
	aliasSrc, err := os.ReadFile(aliasPath) // #nosec G304 -- the path is this program's own flag
	if err != nil {
		return err
	}
	if err := applyAliases(c, aliasSrc); err != nil {
		return err
	}
	goSrc, err := goSource(c)
	if err != nil {
		return err
	}
	jsonSrc, err := jsonCatalog(c)
	if err != nil {
		return err
	}
	for _, out := range []struct {
		path string
		src  []byte
	}{{goPath, goSrc}, {jsonPath, jsonSrc}} {
		// #nosec G306 -- generated source files, committed and read by every build
		if err := os.WriteFile(out.path, out.src, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// verifyPin checks data's SHA-256 against the first field of pin, which is in
// sha256sum's form ("<hex>  <name>").
func verifyPin(data, pin []byte, dataPath string) error {
	fields := strings.Fields(string(pin))
	if len(fields) == 0 {
		return errors.New("the pin file is empty")
	}
	want, err := hex.DecodeString(fields[0])
	if err != nil || len(want) != sha256.Size {
		return fmt.Errorf("the pin file does not start with a SHA-256: %q", fields[0])
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal(sum[:], want) {
		return fmt.Errorf("%s has SHA-256 %x, the pin says %x; the data file is changed only by scripts/emoji-update.sh", dataPath, sum, want)
	}
	return nil
}

// catalog is what the generator reads from emoji-test.txt: the version and
// the fully-qualified sequences, grouped as the file groups them.
type catalog struct {
	Version string
	Groups  []group
}

type group struct {
	Name      string
	Subgroups []subgroup
}

type subgroup struct {
	Name  string
	Emoji []entry
}

// entry is one fully-qualified sequence: the key is its UTF-8 string, the
// name its CLDR short name from the line's comment. Aliases are the typed
// names from aliases.txt (applyAliases); they reach the JSON catalog only.
type entry struct {
	Key     string
	Name    string
	Aliases []string
}

// parse reads the file. Every line is either blank, a comment (`# Version:`,
// `# group:` and `# subgroup:` carry data), or
//
//	<code points> ; <status> # <emoji> E<version> <name>
//
// with the status fully-qualified, minimally-qualified, unqualified or
// component. Only fully-qualified sequences enter the catalog; components
// alone are not emoji and the other two forms canonicalize to the
// fully-qualified one by stripping selectors, which parse checks.
func parse(data []byte) (*catalog, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("the data file is not valid UTF-8")
	}
	c := &catalog{}
	// stripped maps each fully-qualified key with its selectors removed to
	// the key, to find collisions; variants lists every other listed form
	// for the check that each strips to a known key.
	stripped := map[string]string{}
	type variant struct {
		line int
		seq  string
	}
	var variants []variant
	var g *group
	var sg *subgroup
	for n, line := range strings.Split(string(data), "\n") {
		lineNo := n + 1
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "# Version:"):
			if c.Version != "" {
				return nil, fmt.Errorf("line %d: a second # Version: header", lineNo)
			}
			c.Version = strings.TrimSpace(strings.TrimPrefix(line, "# Version:"))
			if c.Version == "" {
				return nil, fmt.Errorf("line %d: an empty # Version: header", lineNo)
			}
			continue
		case strings.HasPrefix(line, "# group:"):
			c.Groups = append(c.Groups, group{Name: strings.TrimSpace(strings.TrimPrefix(line, "# group:"))})
			g = &c.Groups[len(c.Groups)-1]
			sg = nil
			continue
		case strings.HasPrefix(line, "# subgroup:"):
			if g == nil {
				return nil, fmt.Errorf("line %d: a subgroup before any group", lineNo)
			}
			g.Subgroups = append(g.Subgroups, subgroup{Name: strings.TrimSpace(strings.TrimPrefix(line, "# subgroup:"))})
			sg = &g.Subgroups[len(g.Subgroups)-1]
			continue
		case strings.HasPrefix(line, "#"):
			continue
		}
		fields, comment, hasComment := strings.Cut(line, "#")
		if !hasComment {
			return nil, fmt.Errorf("line %d: a data line without its comment", lineNo)
		}
		points, status, hasStatus := strings.Cut(fields, ";")
		if !hasStatus {
			return nil, fmt.Errorf("line %d: a data line without a status", lineNo)
		}
		status = strings.TrimSpace(status)
		var seq strings.Builder
		for _, p := range strings.Fields(points) {
			r, err := parseCodePoint(p)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			seq.WriteRune(r)
		}
		if seq.Len() == 0 {
			return nil, fmt.Errorf("line %d: no code points", lineNo)
		}
		switch status {
		case "fully-qualified":
			if sg == nil {
				return nil, fmt.Errorf("line %d: a sequence before any subgroup", lineNo)
			}
			name, err := shortName(comment)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			key := seq.String()
			s := stripSelectors(key)
			if other, dup := stripped[s]; dup {
				return nil, fmt.Errorf("line %d: fully-qualified sequences %s and %s collide once their presentation selectors are stripped; the lookup table would be ambiguous", lineNo, codePoints(other), codePoints(key))
			}
			stripped[s] = key
			sg.Emoji = append(sg.Emoji, entry{Key: key, Name: name})
		case "minimally-qualified", "unqualified":
			variants = append(variants, variant{line: lineNo, seq: seq.String()})
		case "component":
			// A skin tone or hair component alone is not an emoji.
		default:
			return nil, fmt.Errorf("line %d: unknown status %q", lineNo, status)
		}
	}
	if c.Version == "" {
		return nil, errors.New("no # Version: header")
	}
	if len(stripped) == 0 {
		return nil, errors.New("no fully-qualified sequence")
	}
	for _, v := range variants {
		if _, ok := stripped[stripSelectors(v.seq)]; !ok {
			return nil, fmt.Errorf("line %d: %s strips to no fully-qualified sequence, so it could never canonicalize", v.line, codePoints(v.seq))
		}
	}
	return c, nil
}

// applyAliases reads Conch's alias table (see aliases.txt) and attaches each
// alias to the catalog entry its sequence names. Every alias must name a
// fully-qualified sequence of the pinned data, once selectors are stripped,
// and be unique, or it fails naming the alias and the line. The Go table is
// not touched: aliases are names for typing, never reaction keys.
func applyAliases(c *catalog, src []byte) error {
	byStripped := map[string]*entry{}
	for gi := range c.Groups {
		for si := range c.Groups[gi].Subgroups {
			sg := &c.Groups[gi].Subgroups[si]
			for ei := range sg.Emoji {
				byStripped[stripSelectors(sg.Emoji[ei].Key)] = &sg.Emoji[ei]
			}
		}
	}
	seen := map[string]int{}
	for n, line := range strings.Split(string(src), "\n") {
		lineNo := n + 1
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		alias := fields[0]
		if !validAlias(alias) {
			return fmt.Errorf("aliases line %d: alias %q is not lower-case a-z, 0-9, '_', '+' or '-'", lineNo, alias)
		}
		if first, dup := seen[alias]; dup {
			return fmt.Errorf("aliases line %d: alias %q is already defined on line %d", lineNo, alias, first)
		}
		seen[alias] = lineNo
		if len(fields) < 2 {
			return fmt.Errorf("aliases line %d: alias %q has no code points", lineNo, alias)
		}
		var seq strings.Builder
		for _, p := range fields[1:] {
			r, err := parseCodePoint(p)
			if err != nil {
				return fmt.Errorf("aliases line %d: alias %q: %w", lineNo, alias, err)
			}
			seq.WriteRune(r)
		}
		e, ok := byStripped[stripSelectors(seq.String())]
		if !ok {
			return fmt.Errorf("aliases line %d: alias %q names %s, which is no fully-qualified sequence of the pinned data", lineNo, alias, codePoints(seq.String()))
		}
		e.Aliases = append(e.Aliases, alias)
	}
	return nil
}

func validAlias(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '+' && r != '-' {
			return false
		}
	}
	return true
}

// parseCodePoint reads one hexadecimal code point of a data line: a scalar
// value, so neither a surrogate nor anything past U+10FFFF.
func parseCodePoint(p string) (rune, error) {
	v, err := strconv.ParseUint(p, 16, 32)
	if err != nil || v > utf8.MaxRune {
		return 0, fmt.Errorf("%q is not a code point", p)
	}
	r := rune(v) //nolint:gosec // v is at most utf8.MaxRune, checked above
	if !utf8.ValidRune(r) {
		return 0, fmt.Errorf("%q is not a code point", p)
	}
	return r, nil
}

// shortName reads the CLDR short name from a data line's comment, which is
// "<emoji> E<version> <name>".
func shortName(comment string) (string, error) {
	fields := strings.Fields(comment)
	if len(fields) < 3 || !strings.HasPrefix(fields[1], "E") {
		return "", fmt.Errorf("the comment %q is not \"<emoji> E<version> <name>\"", strings.TrimSpace(comment))
	}
	return strings.Join(fields[2:], " "), nil
}

// stripSelectors removes every U+FE0F and U+FE0E, as CanonicalReactionKey
// does before its lookup.
func stripSelectors(s string) string {
	return strings.Map(func(r rune) rune {
		if r == 0xFE0F || r == 0xFE0E {
			return -1
		}
		return r
	}, s)
}

// codePoints writes s as the file writes a sequence, for messages.
func codePoints(s string) string {
	var parts []string
	for _, r := range s {
		parts = append(parts, fmt.Sprintf("%04X", r))
	}
	return strings.Join(parts, " ")
}

const goHeader = `// Code generated by cmd/emoji-gen from third_party/unicode/emoji-test.txt; DO NOT EDIT.

package schema

// EmojiVersion is the Unicode Emoji version of the catalog in this file: the
// "# Version:" header of the vendored emoji-test.txt. CanonicalReactionKey
// accepts exactly this catalog's sequences, and the browser's catalog,
// web/src/emoji/catalog.json, is generated from the same file in the same run
// and carries the same string (EMOJI_VERSION in web/src/schema/generated.ts).
`

// goSource writes the Go table: every fully-qualified sequence in the file's
// order, as ASCII-only source (escaped string literals), so the generated
// file holds no character a reader cannot see.
func goSource(c *catalog) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(goHeader)
	fmt.Fprintf(&b, "const EmojiVersion = %s\n\n", strconv.QuoteToASCII(c.Version))
	b.WriteString("// emojiCatalog is every fully-qualified emoji sequence of EmojiVersion, in\n")
	b.WriteString("// the file's (CLDR) order, with its CLDR short name, group and subgroup.\n")
	b.WriteString("// Components alone (skin tones, hair) are not listed: they are not emoji.\n")
	b.WriteString("var emojiCatalog = [...]EmojiEntry{\n")
	for _, g := range c.Groups {
		for _, sg := range g.Subgroups {
			for _, e := range sg.Emoji {
				fmt.Fprintf(&b, "\t{Key: %s, Name: %s, Group: %s, Subgroup: %s},\n",
					strconv.QuoteToASCII(e.Key), strconv.QuoteToASCII(e.Name),
					strconv.QuoteToASCII(g.Name), strconv.QuoteToASCII(sg.Name))
			}
		}
	}
	b.WriteString("}\n")
	src, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("the generated Go does not format: %w", err)
	}
	return src, nil
}

// The JSON catalog's shape: the version, then the groups and subgroups in
// the file's order, each emoji with its canonical key and CLDR short name.
// Groups and subgroups with no emoji (the components) are left out.
type jsonDoc struct {
	Version string      `json:"version"`
	Groups  []jsonGroup `json:"groups"`
}

type jsonGroup struct {
	Name      string         `json:"name"`
	Subgroups []jsonSubgroup `json:"subgroups"`
}

type jsonSubgroup struct {
	Name  string      `json:"name"`
	Emoji []jsonEmoji `json:"emoji"`
}

type jsonEmoji struct {
	Key     string   `json:"key"`
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
}

func jsonCatalog(c *catalog) ([]byte, error) {
	doc := jsonDoc{Version: c.Version, Groups: []jsonGroup{}}
	for _, g := range c.Groups {
		jg := jsonGroup{Name: g.Name, Subgroups: []jsonSubgroup{}}
		for _, sg := range g.Subgroups {
			if len(sg.Emoji) == 0 {
				continue
			}
			js := jsonSubgroup{Name: sg.Name, Emoji: make([]jsonEmoji, 0, len(sg.Emoji))}
			for _, e := range sg.Emoji {
				js.Emoji = append(js.Emoji, jsonEmoji(e))
			}
			jg.Subgroups = append(jg.Subgroups, js)
		}
		if len(jg.Subgroups) == 0 {
			continue
		}
		doc.Groups = append(doc.Groups, jg)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	// The file is read by the web build, not by a browser from HTML, so "&"
	// in a group name stays as it is.
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
