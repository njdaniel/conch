// Command gen writes internal/server/names/confusables_table.go from two
// pinned Unicode data files: UTS #39's confusables.txt (each character's
// prototype) and the UCD's DerivedCoreProperties.txt (the
// Default_Ignorable_Code_Point property, which the skeleton drops). It is run
// by `go generate ./internal/server/names` (issue #441).
//
// Each file is named by URL and pinned by SHA-256. A file whose hash differs
// from its pin is refused and nothing is written, so the committed table can
// only ever come from these exact bytes. -confusables and -derived read local
// copies instead of downloading, for a machine without network; the hashes
// are checked the same way. The output is gofmt'd and depends on nothing but
// the input bytes, so a second run writes an identical file.
//
// Moving to another Unicode version means changing the version, both URLs and
// both hashes together, and regenerating. Keys stored in principals.name_key
// are then stale for any name the new data maps differently, and must be
// recomputed by a migration.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The pins. names.TestTablePins reads these constants from this file and
// checks that the committed table records the same ones.
const (
	unicodeVersion    = "18.0.0"
	confusablesURL    = "https://www.unicode.org/Public/18.0.0/security/confusables.txt"
	confusablesSHA256 = "6ed3ee967c9dfdf6677d563c9985182fbc50a2efb7d6059cd57b2e2ce18f5b92"
	derivedURL        = "https://www.unicode.org/Public/18.0.0/ucd/DerivedCoreProperties.txt"
	derivedSHA256     = "09c928886a178fcafd93c29e4bd59073a058e5a100b716d425cb563ab50f68c9"
)

// maxDataFile bounds a download: both files are under 2 MB.
const maxDataFile = 16 << 20

// unicodeLicense is the Unicode License v3 (https://www.unicode.org/license.txt),
// under which both data files are published. Its option (a) asks for this
// notice to go with every copy of the data, so the table carries it.
const unicodeLicense = `UNICODE LICENSE V3

COPYRIGHT AND PERMISSION NOTICE

Copyright © 1991-2026 Unicode, Inc.

NOTICE TO USER: Carefully read the following legal agreement. BY
DOWNLOADING, INSTALLING, COPYING OR OTHERWISE USING DATA FILES, AND/OR
SOFTWARE, YOU UNEQUIVOCALLY ACCEPT, AND AGREE TO BE BOUND BY, ALL OF THE
TERMS AND CONDITIONS OF THIS AGREEMENT. IF YOU DO NOT AGREE, DO NOT
DOWNLOAD, INSTALL, COPY, DISTRIBUTE OR USE THE DATA FILES OR SOFTWARE.

Permission is hereby granted, free of charge, to any person obtaining a
copy of data files and any associated documentation (the "Data Files") or
software and any associated documentation (the "Software") to deal in the
Data Files or Software without restriction, including without limitation
the rights to use, copy, modify, merge, publish, distribute, and/or sell
copies of the Data Files or Software, and to permit persons to whom the
Data Files or Software are furnished to do so, provided that either (a)
this copyright and permission notice appear with all copies of the Data
Files or Software, or (b) this copyright and permission notice appear in
associated Documentation.

THE DATA FILES AND SOFTWARE ARE PROVIDED "AS IS", WITHOUT WARRANTY OF ANY
KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT OF
THIRD PARTY RIGHTS.

IN NO EVENT SHALL THE COPYRIGHT HOLDER OR HOLDERS INCLUDED IN THIS NOTICE
BE LIABLE FOR ANY CLAIM, OR ANY SPECIAL INDIRECT OR CONSEQUENTIAL DAMAGES,
OR ANY DAMAGES WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS,
WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION,
ARISING OUT OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THE DATA
FILES OR SOFTWARE.

Except as contained in this notice, the name of a copyright holder shall
not be used in advertising or otherwise to promote the sale, use or other
dealings in these Data Files or Software without prior written
authorization of the copyright holder.`

func main() {
	out := flag.String("out", "confusables_table.go", "the Go file to write")
	confusablesFile := flag.String("confusables", "", "read confusables.txt from this file instead of downloading it")
	derivedFile := flag.String("derived", "", "read DerivedCoreProperties.txt from this file instead of downloading it")
	flag.Parse()
	if err := run(*out, *confusablesFile, *derivedFile); err != nil {
		fmt.Fprintln(os.Stderr, "names/gen:", err)
		os.Exit(1)
	}
}

func run(out, confusablesFile, derivedFile string) error {
	confusables, err := fetch(confusablesURL, confusablesFile, confusablesSHA256)
	if err != nil {
		return err
	}
	derived, err := fetch(derivedURL, derivedFile, derivedSHA256)
	if err != nil {
		return err
	}
	prototypes, err := parseConfusables(confusables)
	if err != nil {
		return fmt.Errorf("confusables.txt: %w", err)
	}
	ignorable, err := parseDefaultIgnorable(derived)
	if err != nil {
		return fmt.Errorf("DerivedCoreProperties.txt: %w", err)
	}
	src, err := render(prototypes, ignorable)
	if err != nil {
		return err
	}
	return os.WriteFile(out, src, 0o644) // #nosec G306 -- a source file of this repository
}

// fetch returns the bytes of the file at url, or of the local file when one
// is named, and refuses them unless their SHA-256 is want.
func fetch(url, local, want string) ([]byte, error) {
	var data []byte
	var err error
	source := url
	if local != "" {
		source = local + " (standing in for " + url + ")"
		data, err = os.ReadFile(local) // #nosec G304 -- a path the person running the generator gave
	} else {
		data, err = download(url)
	}
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("%s: SHA-256 is %s, pinned %s; refusing it", source, got, want)
	}
	return data, nil
}

func download(url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDataFile+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if len(data) > maxDataFile {
		return nil, fmt.Errorf("download %s: larger than %d bytes", url, maxDataFile)
	}
	return data, nil
}

type prototype struct {
	from rune
	to   string
}

// parseConfusables reads lines of the form "0441 ;\t0063 ;\tMA\t# comment":
// one source code point, then its prototype as code points separated by
// spaces.
func parseConfusables(data []byte) ([]prototype, error) {
	seen := map[rune]bool{}
	var out []prototype
	err := eachDataLine(data, func(fields []string) error {
		if len(fields) < 2 {
			return fmt.Errorf("want at least two fields, got %d", len(fields))
		}
		from, err := codePoints(fields[0])
		if err != nil {
			return err
		}
		if len(from) != 1 {
			return fmt.Errorf("source %q is not one code point", fields[0])
		}
		to, err := codePoints(fields[1])
		if err != nil {
			return err
		}
		if seen[from[0]] {
			return fmt.Errorf("source %04X listed twice", from[0])
		}
		seen[from[0]] = true
		out = append(out, prototype{from: from[0], to: string(to)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("no mappings found")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].from < out[j].from })
	return out, nil
}

type runeRange struct{ lo, hi rune }

// parseDefaultIgnorable collects the code points DerivedCoreProperties.txt
// gives the Default_Ignorable_Code_Point property, as sorted, merged ranges.
func parseDefaultIgnorable(data []byte) ([]runeRange, error) {
	var out []runeRange
	err := eachDataLine(data, func(fields []string) error {
		if len(fields) < 2 || fields[1] != "Default_Ignorable_Code_Point" {
			return nil
		}
		lo, hi, ok := strings.Cut(fields[0], "..")
		if !ok {
			hi = lo
		}
		l, err := codePoints(lo)
		if err != nil || len(l) != 1 {
			return fmt.Errorf("bad range %q", fields[0])
		}
		h, err := codePoints(hi)
		if err != nil || len(h) != 1 || h[0] < l[0] {
			return fmt.Errorf("bad range %q", fields[0])
		}
		out = append(out, runeRange{l[0], h[0]})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("no Default_Ignorable_Code_Point ranges found")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].lo < out[j].lo })
	merged := []runeRange{out[0]}
	for _, r := range out[1:] {
		last := &merged[len(merged)-1]
		if r.lo <= last.hi+1 {
			last.hi = max(last.hi, r.hi)
			continue
		}
		merged = append(merged, r)
	}
	return merged, nil
}

// eachDataLine calls fn with the semicolon-separated, trimmed fields of each
// line of a UCD-format file that carries data, with comments removed.
func eachDataLine(data []byte, fn func(fields []string) error) error {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}) // a UTF-8 byte order mark
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, ";")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		if err := fn(parts); err != nil {
			return fmt.Errorf("line %d: %w", n, err)
		}
	}
	return sc.Err()
}

// codePoints parses space-separated hexadecimal code points.
func codePoints(s string) ([]rune, error) {
	var out []rune
	for _, f := range strings.Fields(s) {
		v, err := strconv.ParseUint(f, 16, 32)
		if err != nil || v > 0x10FFFF || (v >= 0xD800 && v <= 0xDFFF) {
			return nil, fmt.Errorf("bad code point %q", f)
		}
		out = append(out, rune(v))
	}
	if len(out) == 0 {
		return nil, errors.New("empty code point sequence")
	}
	return out, nil
}

func render(prototypes []prototype, ignorable []runeRange) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("// Code generated by internal/server/names/gen; DO NOT EDIT.\n\n")
	b.WriteString("// The tables of the name key's confusable skeleton (UTS #39), Unicode " + unicodeVersion + ".\n//\n")
	b.WriteString("// prototypes is from " + confusablesURL + "\n")
	b.WriteString("// (SHA-256 " + confusablesSHA256 + ").\n")
	b.WriteString("// defaultIgnorable is the Default_Ignorable_Code_Point property of\n")
	b.WriteString("// " + derivedURL + "\n")
	b.WriteString("// (SHA-256 " + derivedSHA256 + ").\n//\n")
	b.WriteString("// Both are Unicode data, used under the Unicode License v3:\n//\n")
	for _, line := range strings.Split(unicodeLicense, "\n") {
		if line == "" {
			b.WriteString("//\n")
			continue
		}
		b.WriteString("//\t" + line + "\n")
	}
	b.WriteString("\npackage names\n\n")
	fmt.Fprintf(&b, "// The Unicode version and the SHA-256 of each source file this table was\n// generated from; TestTablePins checks them against the generator's pins.\nconst (\n")
	fmt.Fprintf(&b, "\ttableUnicodeVersion = %q\n", unicodeVersion)
	fmt.Fprintf(&b, "\ttableConfusablesSHA256 = %q\n", confusablesSHA256)
	fmt.Fprintf(&b, "\ttableDerivedSHA256 = %q\n", derivedSHA256)
	b.WriteString(")\n\n")
	b.WriteString("// prototypes maps each character confusables.txt lists to its prototype,\n// sorted by character.\n")
	b.WriteString("var prototypes = [...]prototype{\n")
	for _, p := range prototypes {
		fmt.Fprintf(&b, "\t{0x%04X, %s},\n", p.from, strconv.QuoteToASCII(p.to))
	}
	b.WriteString("}\n\n")
	b.WriteString("// defaultIgnorable is every Default_Ignorable_Code_Point, as sorted,\n// non-adjacent ranges.\n")
	b.WriteString("var defaultIgnorable = [...]runeRange{\n")
	for _, r := range ignorable {
		fmt.Fprintf(&b, "\t{0x%04X, 0x%04X},\n", r.lo, r.hi)
	}
	b.WriteString("}\n")
	src, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format the table: %w", err)
	}
	return src, nil
}
