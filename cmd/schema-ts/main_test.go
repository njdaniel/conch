package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Small synthetic types, one field per mapping rule. The generator takes the
// TypeScript names from the Go ones, so these names are what the output shows.

type Level string

type Inner struct {
	N int64 `json:"n"`
}

// Unknown is never mapped: Doc reaches it only through a skipped field.
type Unknown struct{}

type Doc struct {
	Schema    string             `json:"schema"`
	S         string             `json:"s"`
	I         int64              `json:"i"`
	Count     int                `json:"count"`
	B         bool               `json:"b"`
	T         time.Time          `json:"t"`
	TS        schema.Timestamp   `json:"ts"`
	J         json.RawMessage    `json:"j"`
	OptJ      json.RawMessage    `json:"opt_j,omitempty"`
	Ptr       *int64             `json:"ptr,omitempty"`
	PtrTS     *schema.Timestamp  `json:"ptr_ts,omitempty"`
	Omit      string             `json:"omit,omitempty"`
	Zero      int64              `json:"zero,omitzero"`
	List      []string           `json:"list"`
	Grid      [][]int64          `json:"grid"`
	Enum      Level              `json:"enum"`
	Enums     []Level            `json:"enums,omitempty"`
	Nested    Inner              `json:"nested"`
	NestedPtr *Inner             `json:"nested_ptr,omitempty"`
	Nesteds   []Inner            `json:"nesteds"`
	Skipped   map[string]Unknown `json:"-"`
	Odd       string             `json:"odd-key"`
	hidden    string             //nolint:unused // there to show an unexported field is skipped
	Shape     *Shape             `json:"shape,omitempty"`
	Shapes    []Shape            `json:"shapes"`
}

type ShapeKind string

// Shape is a union by kind: a circle has a radius, a big box a width and a
// height, and both may have a label.
type Shape struct {
	Kind   ShapeKind `json:"kind"`
	Radius int64     `json:"radius,omitempty"`
	Width  int64     `json:"width,omitempty"`
	Height int64     `json:"height,omitempty"`
	Label  string    `json:"label,omitempty"`
}

var testVocab = map[string][]string{
	"Level":     {"low", "high", "dotted.value"},
	"ShapeKind": {"circle", "big_box"},
}

func shapeUnion() schema.TSUnion {
	return schema.TSUnion{
		Value: Shape{},
		Tag:   "kind",
		Variants: map[string][]string{
			"circle":  {"radius", "label"},
			"big_box": {"width", "height", "label"},
		},
	}
}

// docTypes lists Doc before what it refers to, so the order of the output is
// the generator's doing and not the table's.
func docTypes() []schema.TSType {
	return []schema.TSType{
		{Value: Doc{}, Family: "doc-v1", Schema: "test.doc.v1"},
		{Value: Inner{}, Name: "InnerV2", Family: "inner-v2"},
	}
}

func generateDoc(t *testing.T) string {
	t.Helper()
	files, err := generate(docTypes(), []schema.TSUnion{shapeUnion()}, testVocab, map[reflect.Type]string{
		reflect.TypeFor[Doc](): "test.doc_payload.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(files[typesFile])
}

func TestMapping(t *testing.T) {
	out := generateDoc(t)
	interfaceOf, validatorOf := section(t, out, "export interface Doc {\n", "}\n"), section(t, out, "export const doc = object<Doc>({\n", "});\n")
	tests := []struct {
		rule, field, check string
	}{
		{"schema pinned to the row's Schema", "schema: typeof docSchema;", "schema: vocab<typeof docSchema>({ [docSchema]: true }),"},
		{"string", "s: string;", "s: str,"},
		{"int64", "i: number;", "i: int,"},
		{"int", "count: number;", "count: int,"},
		{"bool", "b: boolean;", "b: bool,"},
		{"time.Time", "t: string;", "t: timestamp,"},
		{"schema.Timestamp", "ts: string;", "ts: timestamp,"},
		{"json.RawMessage", "j: unknown;", "j: json,"},
		{"json.RawMessage, omitempty", "opt_j?: unknown;", "opt_j: optional(json),"},
		{"pointer", "ptr?: number;", "ptr: optional(int),"},
		{"pointer to a timestamp", "ptr_ts?: string;", "ptr_ts: optional(timestamp),"},
		{"omitempty", "omit?: string;", "omit: optional(str),"},
		{"omitzero", "zero?: number;", "zero: optional(int),"},
		{"slice", "list: string[];", "list: array(str),"},
		{"slice of slices", "grid: number[][];", "grid: array(array(int)),"},
		{"named string type", "enum: Level;", "enum: level,"},
		{"slice of a named string type, omitempty", "enums?: Level[];", "enums: optional(array(level)),"},
		{"nested struct, renamed", "nested: InnerV2;", "nested: innerV2,"},
		{"pointer to a nested struct", "nested_ptr?: InnerV2;", "nested_ptr: optional(innerV2),"},
		{"slice of nested structs", "nesteds: InnerV2[];", "nesteds: array(innerV2),"},
		{"a key that is not an identifier", "'odd-key': string;", "'odd-key': str,"},
		{"pointer to a union", "shape?: Shape;", "shape: optional(shape),"},
		{"slice of a union", "shapes: Shape[];", "shapes: array(shape),"},
	}
	for _, tt := range tests {
		t.Run(tt.rule, func(t *testing.T) {
			if !strings.Contains(interfaceOf, "  "+tt.field+"\n") {
				t.Errorf("interface lacks %q:\n%s", tt.field, interfaceOf)
			}
			if !strings.Contains(validatorOf, "  "+tt.check+"\n") {
				t.Errorf("validator lacks %q:\n%s", tt.check, validatorOf)
			}
		})
	}
	t.Run("skipped fields", func(t *testing.T) {
		for _, name := range []string{"Skipped", "skipped", "hidden"} {
			if strings.Contains(interfaceOf, name) || strings.Contains(validatorOf, name) {
				t.Errorf("%s is written:\n%s%s", name, interfaceOf, validatorOf)
			}
		}
		if strings.Contains(out, "Unknown") {
			t.Errorf("a type reached only through a skipped field is written")
		}
	})
	t.Run("keys in json tag order", func(t *testing.T) {
		if strings.Index(interfaceOf, "schema:") > strings.Index(interfaceOf, "shapes:") {
			t.Errorf("keys out of order:\n%s", interfaceOf)
		}
	})
}

func TestDeclarations(t *testing.T) {
	out := generateDoc(t)
	tests := []struct {
		name, want string
	}{
		{"header", "// Code generated by cmd/schema-ts from pkg/schema; DO NOT EDIT.\n"},
		{"imports only what is used, in order", "\nimport { array, bool, int, json, object, optional, str, tagged, timestamp, vocab } from './validate';\n"},
		{"vocabulary", "export type Level = 'low' | 'high' | 'dotted.value';\n\nexport const level = vocab<Level>({ low: true, high: true, 'dotted.value': true });\n"},
		{"schema constant", "export const docSchema = 'test.doc.v1';\n"},
		{"payload constant", "export const docPayloadSchema = 'test.doc_payload.v1';\n"},
		{"renamed struct", "export interface InnerV2 {\n  n: number;\n}\n\nexport const innerV2 = object<InnerV2>({\n  n: int,\n});\n"},
		{"union", `export type Shape = CircleShape | BigBoxShape;

export interface CircleShape {
  kind: 'circle';
  radius: number;
  label: string;
}

export interface BigBoxShape {
  kind: 'big_box';
  width: number;
  height: number;
  label: string;
}

export const shape = tagged<Shape, 'kind'>('kind', {
  circle: object<CircleShape>({
    kind: vocab<'circle'>({ circle: true }),
    radius: int,
    label: str,
  }),
  big_box: object<BigBoxShape>({
    kind: vocab<'big_box'>({ big_box: true }),
    width: int,
    height: int,
    label: str,
  }),
});
`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(out, tt.want) {
				t.Errorf("output lacks\n%s\nin\n%s", tt.want, out)
			}
		})
	}
}

// A validator is a constant: the generated module would fail as it loads if
// one were used above its declaration.
func TestDeclaredBeforeUse(t *testing.T) {
	out := generateDoc(t)
	order := []string{
		"export const level =",
		"export const innerV2 =",
		"export const shapeKind =",
		"export const shape =",
		"export const doc =",
	}
	at := -1
	for _, decl := range order {
		i := strings.Index(out, decl)
		if i < 0 {
			t.Fatalf("no %q in\n%s", decl, out)
		}
		if i < at {
			t.Errorf("%q is declared after something before it in %v", decl, order)
		}
		at = i
	}
}

func TestImportsOnlyWhatIsUsed(t *testing.T) {
	type Small struct {
		Schema string `json:"schema"`
		Name   string `json:"name,omitempty"`
	}
	tests := []struct {
		name  string
		types []schema.TSType
		want  string
	}{
		{"a string", []schema.TSType{{Value: Small{}}}, "import { object, optional, str } from './validate';\n"},
		{"the string pinned by Schema", []schema.TSType{{Value: Small{}, Schema: "x.v1"}}, "import { object, optional, str, vocab } from './validate';\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, err := generate(tt.types, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if out := string(files[typesFile]); !strings.Contains(out, tt.want) {
				t.Errorf("want %q in\n%s", tt.want, out)
			}
		})
	}
}

func TestFamilies(t *testing.T) {
	files, err := generate(docTypes(), []schema.TSUnion{shapeUnion()}, testVocab, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `// Code generated by cmd/schema-ts from pkg/schema/ts.go; DO NOT EDIT.
import type { Validator } from './validate';
import {
  doc,
  innerV2,
} from './generated';

// Each golden fixture family with a generated type, and that type's
// validator. A family is <family>.json and every <family>-*.json.
export const families: [string, Validator<unknown>][] = [
  ['doc-v1', doc],
  ['inner-v2', innerV2],
];
`
	if got := string(files[familiesFile]); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// Generating twice gives the same bytes, whatever order Go's maps iterate in:
// the drift gate compares bytes.
func TestDeterministic(t *testing.T) {
	first := generateDoc(t)
	for range 20 {
		if again := generateDoc(t); again != first {
			t.Fatalf("output changed between runs")
		}
	}
}

// The real tables generate, so a broken row fails go test as well as the
// drift gate.
func TestRealTables(t *testing.T) {
	files, err := generate(schema.TSTypes, schema.TSUnions, schema.Vocabularies, payloadNames())
	if err != nil {
		t.Fatal(err)
	}
	out := string(files[typesFile])
	for _, want := range []string{
		"export const approvalV1PayloadSchema = 'conch.approval.v1';",
		"export const messageV2Schema = 'conch.message.v2';",
		"export interface ErrorV0 {",
		"export type Audience = NetAudience | PrincipalsAudience;",
		"export type EscalationTarget = PrincipalEscalationTarget | NtfyTopicEscalationTarget;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

func TestRefused(t *testing.T) {
	type NoTag struct {
		Name string
	}
	type Bytes struct {
		B []byte `json:"b"`
	}
	type Map struct {
		M map[string]string `json:"m"`
	}
	type Uint struct {
		U uint64 `json:"u"`
	}
	type Float struct {
		F float64 `json:"f"`
	}
	type Iface struct {
		A any `json:"a"`
	}
	type StringOpt struct {
		I int64 `json:"i,string"`
	}
	type PtrPtr struct {
		P **int64 `json:"p,omitempty"`
	}
	type BarePtr struct {
		P *int64 `json:"p"`
	}
	type Proto struct {
		P string `json:"__proto__"`
	}
	type Embedded struct {
		Inner
	}
	// Built at run time: go vet refuses a struct literal with a repeated tag.
	twice := reflect.New(reflect.StructOf([]reflect.StructField{
		{Name: "A", Type: reflect.TypeFor[string](), Tag: `json:"a"`},
		{Name: "B", Type: reflect.TypeFor[string](), Tag: `json:"a"`},
	})).Elem().Interface()
	type Unlisted struct {
		I Inner `json:"i"`
	}
	type Colour string
	type NoVocab struct {
		K Colour `json:"k"`
	}
	type NoSchemaField struct {
		ID int64 `json:"id"`
	}
	type OptionalSchema struct {
		Schema string `json:"schema,omitempty"`
	}
	type Self struct {
		Next []Self `json:"next"`
	}
	type Word struct {
		S string `json:"s"`
	}
	type Plain struct {
		S string `json:"ké"`
	}
	type Kindless struct {
		Kind string `json:"kind"`
	}
	circle := func(variants map[string][]string) []schema.TSUnion {
		return []schema.TSUnion{{Value: Shape{}, Tag: "kind", Variants: variants}}
	}
	tests := []struct {
		name   string
		types  []schema.TSType
		unions []schema.TSUnion
		want   string
	}{
		{"a field without a json name", []schema.TSType{{Value: NoTag{}}}, nil, "NoTag.Name: no json name"},
		{"[]byte", []schema.TSType{{Value: Bytes{}}}, nil, "Bytes.B: []uint8 is base64"},
		{"a map", []schema.TSType{{Value: Map{}}}, nil, "Map.M: map[string]string is not a type this generator maps"},
		{"an unsigned int", []schema.TSType{{Value: Uint{}}}, nil, "Uint.U: uint64 is not a type"},
		{"a float", []schema.TSType{{Value: Float{}}}, nil, "Float.F: float64 is not a type"},
		{"an interface", []schema.TSType{{Value: Iface{}}}, nil, "Iface.A: interface {} is not a type"},
		{"the string option", []schema.TSType{{Value: StringOpt{}}}, nil, `StringOpt.I: the json option "string"`},
		{"a pointer to a pointer", []schema.TSType{{Value: PtrPtr{}}}, nil, "PtrPtr.P: *int64 is not a type"},
		{"a pointer without omitempty", []schema.TSType{{Value: BarePtr{}}}, nil, "BarePtr.P: a pointer without omitempty or omitzero is null when nil"},
		{"__proto__ as a key", []schema.TSType{{Value: Proto{}}}, nil, "Proto.P: __proto__ cannot be an object key"},
		{"a reserved word as a name", []schema.TSType{{Value: Word{}, Name: "Default"}}, nil, `"default" is a reserved word`},
		{"a TypeScript type name as a name", []schema.TSType{{Value: Word{}, Name: "String"}}, nil, `"string" is a reserved word`},
		{"an embedded struct", []schema.TSType{{Value: Embedded{}}}, nil, "Embedded.Inner: an embedded struct"},
		{"a key used twice", []schema.TSType{{Value: twice, Name: "Twice"}}, nil, `.B: json key "a" is used twice`},
		{"a struct not listed", []schema.TSType{{Value: Unlisted{}}}, nil, "Unlisted.I: main.Inner is not in schema.TSTypes"},
		{"a named string type with no vocabulary", []schema.TSType{{Value: NoVocab{}}}, nil, "NoVocab.K: Colour is a named string type not in schema.Vocabularies"},
		{"Schema without a schema field", []schema.TSType{{Value: NoSchemaField{}, Schema: "x.v1"}}, nil, "NoSchemaField: Schema is set"},
		{"Schema on an optional field", []schema.TSType{{Value: OptionalSchema{}, Schema: "x.v1"}}, nil, "OptionalSchema: Schema is set"},
		{"a struct that contains itself", []schema.TSType{{Value: Self{}}}, nil, "Self contains itself"},
		{"a value that is not a struct", []schema.TSType{{Value: "x"}}, nil, "must be a struct"},
		{"a pointer value", []schema.TSType{{Value: &Word{}}}, nil, "must be a struct"},
		{"a type listed twice", []schema.TSType{{Value: Word{}}, {Value: Word{}}}, nil, "Word is listed twice"},
		{"a family listed twice", []schema.TSType{{Value: Word{}, Family: "f"}, {Value: Inner{}, Family: "f"}}, nil, `fixture family "f" is listed twice`},
		{"two types with one TypeScript name", []schema.TSType{{Value: Word{}}, {Value: Inner{}, Name: "Word"}}, nil, "the name Word is already Word's"},
		{"a name that is a combinator", []schema.TSType{{Value: Word{}, Name: "Json"}}, nil, "the name json is already validate.ts's"},
		{"a name that is not an identifier", []schema.TSType{{Value: Word{}, Name: "Not-a-name"}}, nil, "is not a TypeScript identifier"},
		{"a key outside printable ASCII", []schema.TSType{{Value: Plain{}}}, nil, "is not printable ASCII"},
		{"a schema outside printable ASCII", []schema.TSType{{Value: Word{}, Schema: "x\n"}}, nil, "is not printable ASCII"},
		{"a variant for a value the tag lacks", nil, circle(map[string][]string{"circle": {"radius", "label"}, "big_box": {"width", "height", "label"}, "cone": {}}), `variant "cone" is not a value of ShapeKind`},
		{"a value of the tag with no variant", nil, circle(map[string][]string{"circle": {"radius", "width", "height", "label"}}), `ShapeKind "big_box" has no variant`},
		{"a field in no variant", nil, circle(map[string][]string{"circle": {"radius"}, "big_box": {"width", "height"}}), "Shape.label is in no variant"},
		{"a variant listing an unknown field", nil, circle(map[string][]string{"circle": {"radius", "label", "colour"}, "big_box": {"width", "height"}}), `variant "circle" lists "colour"`},
		{"a variant listing the tag", nil, circle(map[string][]string{"circle": {"kind", "radius", "label"}, "big_box": {"width", "height"}}), `variant "circle" lists "kind"`},
		{"a tag that is not a field", nil, []schema.TSUnion{{Value: Shape{}, Tag: "type", Variants: map[string][]string{}}}, `the union's tag "type" is not one of its fields`},
		{"a tag that is not a vocabulary", nil, []schema.TSUnion{{Value: Kindless{}, Tag: "kind", Variants: map[string][]string{}}}, "a union's tag must be a type in Vocabularies"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := generate(tt.types, tt.unions, testVocab, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// A vocabulary member is an object key in vocab({...}), where __proto__
// would set the prototype instead.
func TestRefusedVocabulary(t *testing.T) {
	type Leveled struct {
		L Level `json:"l"`
	}
	_, err := generate([]schema.TSType{{Value: Leveled{}}}, nil, map[string][]string{"Level": {"low", "__proto__"}}, nil)
	if want := "Vocabularies[Level]: __proto__ cannot be an object key"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want one containing %q", err, want)
	}
}

func TestNames(t *testing.T) {
	tests := []struct{ in, lower, pascal string }{
		{"RunV1", "runV1", "RunV1"},
		{"WSTicketV1", "wsTicketV1", "WSTicketV1"},
		{"WhoAmIResponseV1", "whoAmIResponseV1", "WhoAmIResponseV1"},
		{"ID", "id", "ID"},
		{"net", "net", "Net"},
		{"ntfy_topic", "ntfy_topic", "NtfyTopic"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := lowerFirst(tt.in); got != tt.lower {
				t.Errorf("lowerFirst(%q) = %q, want %q", tt.in, got, tt.lower)
			}
			if got := pascal(tt.in); got != tt.pascal {
				t.Errorf("pascal(%q) = %q, want %q", tt.in, got, tt.pascal)
			}
		})
	}
}

// section returns the part of out from start to the first end after it.
func section(t *testing.T, out, start, end string) string {
	t.Helper()
	i := strings.Index(out, start)
	if i < 0 {
		t.Fatalf("no %q in\n%s", start, out)
	}
	j := strings.Index(out[i:], end)
	if j < 0 {
		t.Fatalf("no %q after %q", end, start)
	}
	return out[i : i+j+len(end)]
}
