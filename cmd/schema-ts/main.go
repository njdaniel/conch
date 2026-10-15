// Command schema-ts writes the web client's wire types from pkg/schema (issue
// #348, CLAUDE.md rule 2): web/src/schema/generated.ts, one TypeScript type
// and one validator for each struct in schema.TSTypes and schema.TSUnions, and
// generated.families.ts, the golden fixture families the web test checks
// those validators against. `go generate ./pkg/schema` runs it;
// scripts/schema-ts-check.sh runs it into a temporary directory and fails
// when the committed files differ.
//
// It reads the Go types by reflection and needs nothing but Go. A field's
// name is its json tag; its type maps as follows:
//
//	string                         str
//	int, int64 (any signed int)    int
//	bool                           bool
//	time.Time, schema.Timestamp    timestamp
//	json.RawMessage                json
//	a named string type            vocab over its schema.Vocabularies entry
//	[]T                            array(T)
//	a struct                       that struct's validator, which must be listed
//	omitempty or omitzero          optional(T)
//	*T, with omitempty or omitzero optional(T); a bare *T is refused, as Go
//	                               writes a nil one as null
//	json:"-" or unexported         skipped
//
// Anything else is refused, with the field it is in, rather than guessed at.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/njdaniel/conch/pkg/schema"
)

// The files this program writes, in the directory -out names.
const (
	typesFile    = "generated.ts"
	familiesFile = "generated.families.ts"
)

func main() {
	out := flag.String("out", "", "the directory to write "+typesFile+" and "+familiesFile+" into")
	flag.Parse()
	if *out == "" || flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: schema-ts -out <dir>")
		os.Exit(2)
	}
	files, err := generate(schema.TSTypes, schema.TSUnions, schema.Vocabularies, payloadNames())
	if err != nil {
		fmt.Fprintln(os.Stderr, "schema-ts:", err)
		os.Exit(1)
	}
	for _, name := range []string{typesFile, familiesFile} {
		// #nosec G306 -- a source file of the web client, committed and read by every build
		if err := os.WriteFile(filepath.Join(*out, name), files[name], 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "schema-ts:", err)
			os.Exit(1)
		}
	}
}

// payloadNames is the payload schema name each registered payload type is
// carried under, by its Go type: a message's payload names the type of its
// data this way, so the web client needs the name as well as the type.
func payloadNames() map[reflect.Type]string {
	names := map[reflect.Type]string{}
	for _, name := range schema.RegisteredPayloads() {
		s, _ := schema.LookupPayload(name)
		names[reflect.TypeOf(s.New()).Elem()] = name
	}
	return names
}

var (
	timeType      = reflect.TypeFor[time.Time]()
	timestampType = reflect.TypeFor[schema.Timestamp]()
	rawType       = reflect.TypeFor[json.RawMessage]()
)

// combinators is what validate.ts exports for generated code to use, in the
// order the import names them. No generated name may be one of them.
var combinators = []string{"array", "bool", "int", "json", "object", "optional", "str", "tagged", "timestamp", "vocab"}

// decl is one struct to write: a TSTypes or a TSUnions entry.
type decl struct {
	typ     reflect.Type
	name    string // the TypeScript type's name
	value   string // the validator's name
	family  string
	schema  string
	payload string
	union   *schema.TSUnion
	state   int // unwritten, writing or written
}

const (
	unwritten = iota
	writing
	written
)

// expr is how TypeScript reads a value of one Go type.
type expr struct {
	ts    string   // its TypeScript type
	check string   // its validator
	uses  []string // the combinators check calls
}

// field is one json key of a struct, as TypeScript sees it.
type field struct {
	expr
	key      string
	optional bool
	goType   reflect.Type
}

type generator struct {
	vocab  map[string][]string
	decls  map[reflect.Type]*decl
	vocabs map[string]bool   // the vocabularies written so far
	names  map[string]string // each name declared in generated.ts, and whose it is
	used   map[string]bool   // the combinators the declarations call
	body   strings.Builder
}

// generate returns the content of each file this program writes, by name.
func generate(types []schema.TSType, unions []schema.TSUnion, vocab map[string][]string, payloads map[reflect.Type]string) (map[string][]byte, error) {
	g := &generator{
		vocab:  vocab,
		decls:  map[reflect.Type]*decl{},
		vocabs: map[string]bool{},
		names:  map[string]string{},
		used:   map[string]bool{},
	}
	for _, c := range combinators {
		g.names[c] = "validate.ts"
	}
	if err := plainTables(types, unions, vocab, payloads); err != nil {
		return nil, err
	}
	var order []*decl
	families := map[string]bool{}
	for _, t := range types {
		d, err := g.declare(t.Value, t.Name)
		if err != nil {
			return nil, err
		}
		d.family, d.schema, d.payload = t.Family, t.Schema, payloads[d.typ]
		if d.family != "" {
			if families[d.family] {
				return nil, fmt.Errorf("fixture family %q is listed twice", d.family)
			}
			families[d.family] = true
		}
		order = append(order, d)
	}
	for i := range unions {
		d, err := g.declare(unions[i].Value, "")
		if err != nil {
			return nil, err
		}
		d.union = &unions[i]
		order = append(order, d)
	}
	for _, d := range order {
		if err := g.write(d); err != nil {
			return nil, err
		}
	}

	var out strings.Builder
	out.WriteString(header)
	var imports []string
	for _, c := range combinators {
		if g.used[c] {
			imports = append(imports, c)
		}
	}
	fmt.Fprintf(&out, "import { %s } from './validate';\n", strings.Join(imports, ", "))
	out.WriteString(g.body.String())
	return map[string][]byte{
		typesFile:    []byte(out.String()),
		familiesFile: familiesTS(order),
	}, nil
}

// familiesTS writes generated.families.ts: each listed type that has fixtures,
// in table order.
func familiesTS(order []*decl) []byte {
	var withFamily []*decl
	for _, d := range order {
		if d.family != "" {
			withFamily = append(withFamily, d)
		}
	}
	var b strings.Builder
	b.WriteString(familiesHeader)
	b.WriteString("import type { Validator } from './validate';\nimport {\n")
	sorted := slices.Clone(withFamily)
	slices.SortFunc(sorted, func(a, b *decl) int { return strings.Compare(a.value, b.value) })
	for _, d := range sorted {
		fmt.Fprintf(&b, "  %s,\n", d.value)
	}
	b.WriteString("} from './generated';\n\n")
	b.WriteString("// Each golden fixture family with a generated type, and that type's\n")
	b.WriteString("// validator. A family is <family>.json and every <family>-*.json.\n")
	b.WriteString("export const families: [string, Validator<unknown>][] = [\n")
	for _, d := range withFamily {
		fmt.Fprintf(&b, "  [%s, %s],\n", quote(d.family), d.value)
	}
	b.WriteString("];\n")
	return []byte(b.String())
}

// plainTables refuses any string in the tables that is not printable ASCII,
// before any of them is written into TypeScript.
func plainTables(types []schema.TSType, unions []schema.TSUnion, vocab map[string][]string, payloads map[reflect.Type]string) error {
	var errs []error
	for _, t := range types {
		errs = append(errs, plain(t.Family, "a TSTypes Family"), plain(t.Schema, "a TSTypes Schema"))
	}
	for _, u := range unions {
		errs = append(errs, plain(u.Tag, "a TSUnions Tag"))
		for tag, keys := range u.Variants {
			errs = append(errs, plain(tag, "a TSUnions variant"))
			for _, k := range keys {
				errs = append(errs, plain(k, "a TSUnions variant's field"))
			}
		}
	}
	for name, members := range vocab {
		for _, m := range members {
			errs = append(errs, plainKey(m, "Vocabularies["+name+"]"))
		}
	}
	for _, name := range payloads {
		errs = append(errs, plain(name, "a payload name"))
	}
	return errors.Join(errs...)
}

const header = `// Code generated by cmd/schema-ts from pkg/schema; DO NOT EDIT.
//
// The web client's wire types: a TypeScript type and a validator for each Go
// struct in pkg/schema/ts.go's tables, built from the combinators in
// validate.ts. To change one, change the Go type or the table and run
// ` + "`go generate ./pkg/schema`" + `; scripts/schema-ts-check.sh fails while this file
// differs from what that writes.
`

const familiesHeader = `// Code generated by cmd/schema-ts from pkg/schema/ts.go; DO NOT EDIT.
`

// declare records a struct to be written, under its TypeScript names.
func (g *generator) declare(value any, name string) (*decl, error) {
	t := reflect.TypeOf(value)
	if t == nil || t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%v: a table entry's Value must be a struct, not %v", value, t)
	}
	if _, dup := g.decls[t]; dup {
		return nil, fmt.Errorf("%s is listed twice", t.Name())
	}
	if name == "" {
		name = t.Name()
	}
	d := &decl{typ: t, name: name, value: lowerFirst(name)}
	if err := g.claim(d.name, t.Name()); err != nil {
		return nil, err
	}
	if err := g.claim(d.value, t.Name()); err != nil {
		return nil, err
	}
	g.decls[t] = d
	return d, nil
}

// claim reserves a name in generated.ts, so two declarations, or a
// declaration and a combinator, never share one.
func (g *generator) claim(name, owner string) error {
	if !identifier.MatchString(name) {
		return fmt.Errorf("%s: %q is not a TypeScript identifier", owner, name)
	}
	if reserved[name] {
		return fmt.Errorf("%s: %q is a reserved word in TypeScript", owner, name)
	}
	if other, taken := g.names[name]; taken {
		return fmt.Errorf("%s: the name %s is already %s's", owner, name, other)
	}
	g.names[name] = owner
	return nil
}

// write writes d, after everything it refers to: a validator is a constant,
// and one used before it is declared would be undefined when the module runs.
func (g *generator) write(d *decl) error {
	switch d.state {
	case written:
		return nil
	case writing:
		return fmt.Errorf("%s contains itself; a validator cannot", d.typ.Name())
	}
	d.state = writing
	var err error
	if d.union != nil {
		err = g.writeUnion(d)
	} else {
		err = g.writeStruct(d)
	}
	d.state = written
	return err
}

func (g *generator) writeStruct(d *decl) error {
	fields, err := g.fields(d.typ)
	if err != nil {
		return err
	}
	b := &g.body
	fmt.Fprintf(b, "\n// schema.%s\n", d.typ.Name())
	if d.payload != "" {
		fmt.Fprintf(b, "// A message payload of schema %sPayloadSchema carries one as its data.\n", d.value)
		fmt.Fprintf(b, "export const %sPayloadSchema = %s;\n\n", d.value, quote(d.payload))
		if err := g.claim(d.value+"PayloadSchema", d.typ.Name()); err != nil {
			return err
		}
	}
	if d.schema != "" {
		name := d.value + "Schema"
		if err := g.claim(name, d.typ.Name()); err != nil {
			return err
		}
		i := slices.IndexFunc(fields, func(f field) bool { return f.key == "schema" })
		if i < 0 || fields[i].goType != reflect.TypeFor[string]() || fields[i].optional {
			return fmt.Errorf("%s: Schema is set, so it needs a required string field tagged json:\"schema\"", d.typ.Name())
		}
		fields[i].expr = expr{
			ts:    "typeof " + name,
			check: fmt.Sprintf("vocab<typeof %s>({ [%s]: true })", name, name),
			uses:  []string{"vocab"},
		}
		fmt.Fprintf(b, "export const %s = %s;\n\n", name, quote(d.schema))
	}
	g.writeInterface(d.name, fields, "")
	b.WriteString("\n")
	fmt.Fprintf(b, "export const %s = ", d.value)
	g.writeObject(d.name, fields, "")
	b.WriteString(";\n")
	return nil
}

func (g *generator) writeInterface(name string, fields []field, indent string) {
	b := &g.body
	fmt.Fprintf(b, "%sexport interface %s {\n", indent, name)
	for _, f := range fields {
		opt := ""
		if f.optional {
			opt = "?"
		}
		fmt.Fprintf(b, "%s  %s%s: %s;\n", indent, key(f.key), opt, f.ts)
	}
	fmt.Fprintf(b, "%s}\n", indent)
}

// writeObject writes object<name>({...}), indented by indent after its first
// line.
func (g *generator) writeObject(name string, fields []field, indent string) {
	g.used["object"] = true
	b := &g.body
	fmt.Fprintf(b, "object<%s>({\n", name)
	for _, f := range fields {
		check := f.check
		for _, c := range f.uses {
			g.used[c] = true
		}
		if f.optional {
			g.used["optional"] = true
			check = "optional(" + check + ")"
		}
		fmt.Fprintf(b, "%s  %s: %s,\n", indent, key(f.key), check)
	}
	fmt.Fprintf(b, "%s})", indent)
}

func (g *generator) writeUnion(d *decl) error {
	u := d.union
	fields, err := g.fields(d.typ)
	if err != nil {
		return err
	}
	tagAt := slices.IndexFunc(fields, func(f field) bool { return f.key == u.Tag })
	if tagAt < 0 {
		return fmt.Errorf("%s: the union's tag %q is not one of its fields", d.typ.Name(), u.Tag)
	}
	tagType := fields[tagAt].goType
	tags, ok := g.vocab[tagType.Name()]
	if tagType.Kind() != reflect.String || tagType.Name() == "string" || !ok {
		return fmt.Errorf("%s.%s: a union's tag must be a type in Vocabularies", d.typ.Name(), u.Tag)
	}
	for tag := range u.Variants {
		if !slices.Contains(tags, tag) {
			return fmt.Errorf("%s: variant %q is not a value of %s", d.typ.Name(), tag, tagType.Name())
		}
	}
	inSome := map[string]bool{u.Tag: true}
	type variant struct {
		tag, name string
		fields    []field
	}
	var variants []variant
	for _, tag := range tags {
		keys, ok := u.Variants[tag]
		if !ok {
			return fmt.Errorf("%s: %s %q has no variant", d.typ.Name(), tagType.Name(), tag)
		}
		v := variant{tag: tag, name: pascal(tag) + d.name}
		if err := g.claim(v.name, d.typ.Name()); err != nil {
			return err
		}
		v.fields = append(v.fields, field{key: u.Tag, expr: expr{
			ts:    quote(tag),
			check: fmt.Sprintf("vocab<%s>({ %s: true })", quote(tag), key(tag)),
			uses:  []string{"vocab"},
		}})
		for _, f := range fields {
			if f.key == u.Tag {
				continue
			}
			if slices.Contains(keys, f.key) {
				f.optional = false
				v.fields = append(v.fields, f)
				inSome[f.key] = true
			}
		}
		for _, k := range keys {
			if !slices.ContainsFunc(fields, func(f field) bool { return f.key == k }) || k == u.Tag {
				return fmt.Errorf("%s: variant %q lists %q, which is not one of its other fields", d.typ.Name(), tag, k)
			}
		}
		variants = append(variants, v)
	}
	for _, f := range fields {
		if !inSome[f.key] {
			return fmt.Errorf("%s.%s is in no variant of the union", d.typ.Name(), f.key)
		}
	}

	b := &g.body
	names := make([]string, len(variants))
	for i, v := range variants {
		names[i] = v.name
	}
	fmt.Fprintf(b, "\n// schema.%s, by %s.\n", d.typ.Name(), u.Tag)
	fmt.Fprintf(b, "export type %s = %s;\n", d.name, strings.Join(names, " | "))
	for _, v := range variants {
		b.WriteString("\n")
		g.writeInterface(v.name, v.fields, "")
	}
	g.used["tagged"] = true
	fmt.Fprintf(b, "\nexport const %s = tagged<%s, %s>(%s, {\n", d.value, d.name, quote(u.Tag), quote(u.Tag))
	for _, v := range variants {
		fmt.Fprintf(b, "  %s: ", key(v.tag))
		g.writeObject(v.name, v.fields, "  ")
		b.WriteString(",\n")
	}
	b.WriteString("});\n")
	return nil
}

// fields reads a struct's json keys in declaration order, writing anything a
// key's type refers to that is not yet written.
func (g *generator) fields(t reflect.Type) ([]field, error) {
	var out []field
	seen := map[string]bool{}
	for i := range t.NumField() {
		sf := t.Field(i)
		where := t.Name() + "." + sf.Name
		if sf.Anonymous {
			return nil, fmt.Errorf("%s: an embedded struct is not supported; name the field", where)
		}
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			return nil, fmt.Errorf("%s: no json name; a key's name comes from its json tag alone", where)
		}
		if seen[name] {
			return nil, fmt.Errorf("%s: json key %q is used twice", where, name)
		}
		if err := plainKey(name, where); err != nil {
			return nil, err
		}
		seen[name] = true
		f := field{key: name, goType: sf.Type}
		for _, opt := range strings.Split(opts, ",") {
			switch opt {
			case "":
			case "omitempty", "omitzero":
				f.optional = true
			default:
				return nil, fmt.Errorf("%s: the json option %q is not supported", where, opt)
			}
		}
		ft := sf.Type
		if ft.Kind() == reflect.Pointer {
			// A nil pointer is written as null unless the tag omits it, and
			// TypeScript's optional key cannot be null.
			if !f.optional {
				return nil, fmt.Errorf("%s: a pointer without omitempty or omitzero is null when nil, which is not supported", where)
			}
			ft = ft.Elem()
			f.goType = ft
		}
		e, err := g.ref(ft, where)
		if err != nil {
			return nil, err
		}
		f.expr = e
		out = append(out, f)
	}
	return out, nil
}

// ref returns how TypeScript reads a value of Go type t.
func (g *generator) ref(t reflect.Type, where string) (expr, error) {
	switch t {
	case timeType, timestampType:
		return expr{"string", "timestamp", []string{"timestamp"}}, nil
	case rawType:
		return expr{"unknown", "json", []string{"json"}}, nil
	}
	switch t.Kind() {
	case reflect.String:
		if t.PkgPath() == "" {
			return expr{"string", "str", []string{"str"}}, nil
		}
		return g.vocabulary(t, where)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return expr{"number", "int", []string{"int"}}, nil
	case reflect.Bool:
		return expr{"boolean", "bool", []string{"bool"}}, nil
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return expr{}, fmt.Errorf("%s: %v is base64 on the wire, which is not supported", where, t)
		}
		e, err := g.ref(t.Elem(), where)
		if err != nil {
			return expr{}, err
		}
		return expr{e.ts + "[]", "array(" + e.check + ")", append(e.uses, "array")}, nil
	case reflect.Struct:
		d, ok := g.decls[t]
		if !ok {
			return expr{}, fmt.Errorf("%s: %v is not in schema.TSTypes or schema.TSUnions", where, t)
		}
		if err := g.write(d); err != nil {
			return expr{}, err
		}
		return expr{ts: d.name, check: d.value}, nil
	default:
		return expr{}, fmt.Errorf("%s: %v is not a type this generator maps", where, t)
	}
}

// vocabulary writes, once, the union type and validator of a named string
// type, and returns their names.
func (g *generator) vocabulary(t reflect.Type, where string) (expr, error) {
	name := t.Name()
	members, ok := g.vocab[name]
	if !ok {
		return expr{}, fmt.Errorf("%s: %s is a named string type not in schema.Vocabularies", where, name)
	}
	value := lowerFirst(name)
	if g.vocabs[name] {
		return expr{ts: name, check: value}, nil
	}
	if len(members) == 0 {
		return expr{}, fmt.Errorf("%s: vocabulary %s is empty", where, name)
	}
	if err := g.claim(name, name); err != nil {
		return expr{}, err
	}
	if err := g.claim(value, name); err != nil {
		return expr{}, err
	}
	g.vocabs[name] = true
	g.used["vocab"] = true
	lits := make([]string, len(members))
	entries := make([]string, len(members))
	for i, m := range members {
		lits[i] = quote(m)
		entries[i] = key(m) + ": true"
	}
	b := &g.body
	fmt.Fprintf(b, "\n// schema.%s\n", name)
	if line := fmt.Sprintf("export type %s = %s;", name, strings.Join(lits, " | ")); len(line) <= maxLine {
		b.WriteString(line + "\n")
	} else {
		fmt.Fprintf(b, "export type %s =\n  | %s;\n", name, strings.Join(lits, "\n  | "))
	}
	b.WriteString("\n")
	if line := fmt.Sprintf("export const %s = vocab<%s>({ %s });", value, name, strings.Join(entries, ", ")); len(line) <= maxLine {
		b.WriteString(line + "\n")
	} else {
		fmt.Fprintf(b, "export const %s = vocab<%s>({\n  %s,\n});\n", value, name, strings.Join(entries, ",\n  "))
	}
	return expr{ts: name, check: value}, nil
}

// reserved is every word TypeScript refuses as the name of a constant or a
// type: JavaScript's reserved words, strict mode's, and TypeScript's own type
// names. A Go struct Default would otherwise become "export const default".
var reserved = func() map[string]bool {
	words := map[string]bool{}
	for _, w := range strings.Fields(`
		break case catch class const continue debugger default delete do else
		enum export extends false finally for function if import in instanceof
		new null return super switch this throw true try typeof var void while
		with implements interface let package private protected public static
		yield await any unknown never object string number boolean symbol
		bigint undefined`) {
		words[w] = true
	}
	return words
}()

// maxLine is the length past which a declaration is written one member to a
// line.
const maxLine = 100

var (
	identifier = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
	printable  = regexp.MustCompile(`^[\x20-\x7e]*$`)
)

// quote writes s as a TypeScript string literal. generate has refused any
// string outside printable ASCII before it gets here, so escaping the quote
// and the backslash is enough.
func quote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// plainKey is plain for a string written as an object key, which also may
// not be __proto__: in an object literal that sets the prototype instead of
// adding a key, so vocab() and object() would never see it.
func plainKey(s, where string) error {
	if s == "__proto__" {
		return fmt.Errorf("%s: __proto__ cannot be an object key", where)
	}
	return plain(s, where)
}

// plain refuses a string from a table or a tag that is not printable ASCII:
// it is a mistake there, and is refused rather than escaped.
func plain(s, where string) error {
	if !printable.MatchString(s) {
		return fmt.Errorf("%s: %q is not printable ASCII", where, s)
	}
	return nil
}

// key writes s as an object key: bare when it is an identifier.
func key(s string) string {
	if identifier.MatchString(s) {
		return s
	}
	return quote(s)
}

// lowerFirst is the validator's name for a type: its leading capitals in
// lower case, but for the one that starts the next word (WSTicketV1 is
// wsTicketV1, WhoAmIResponseV1 whoAmIResponseV1).
func lowerFirst(s string) string {
	r := []rune(s)
	n := 0
	for n < len(r) && unicode.IsUpper(r[n]) {
		n++
	}
	if n > 1 && n < len(r) && unicode.IsLower(r[n]) {
		n--
	}
	for i := range n {
		r[i] = unicode.ToLower(r[i])
	}
	return string(r)
}

// pascal turns a tag value into the start of its variant's name: net is Net,
// ntfy_topic NtfyTopic.
func pascal(s string) string {
	var b strings.Builder
	for _, part := range strings.Split(s, "_") {
		if part != "" {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String()
}
