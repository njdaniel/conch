package schema

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// source is what vocabularies_test reads from this package's non-test files:
// every string type, the constants declared of each, and the values each
// type's Valid() switch accepts.
type source struct {
	stringTypes map[string]bool
	// consts is each type's constants' values, in declaration order (files in
	// name order).
	consts map[string][]string
	// valid is the values each type's Valid() switch accepts, for the types
	// that have one.
	valid map[string][]string
	// names is every unexported func named <type>Names, by its type's name.
	names map[string]bool
}

// readSource parses the package's non-test files. Valid() is read as a switch
// whose cases are this package's constants or string literals, which is how
// every one is written; one that is not fails the test, which cannot then
// tell what it accepts.
func readSource(t *testing.T) source {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	src := source{
		stringTypes: map[string]bool{},
		consts:      map[string][]string{},
		valid:       map[string][]string{},
		names:       map[string]bool{},
	}
	fset := token.NewFileSet()
	var parsed []*ast.File
	constValue := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, f)
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					if id, ok := spec.Type.(*ast.Ident); ok && id.Name == "string" && spec.Assign == 0 {
						src.stringTypes[spec.Name.Name] = true
					}
				case *ast.ValueSpec:
					typ, ok := spec.Type.(*ast.Ident)
					if gen.Tok != token.CONST || !ok {
						continue
					}
					for i, v := range spec.Values {
						lit, ok := v.(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						s, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatal(err)
						}
						src.consts[typ.Name] = append(src.consts[typ.Name], s)
						constValue[spec.Names[i].Name] = s
					}
				}
			}
		}
	}
	for _, f := range parsed {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Recv == nil {
				base, ok := strings.CutSuffix(fn.Name.Name, "Names")
				if ok && base != "" && !ast.IsExported(fn.Name.Name) && src.stringTypes[strings.ToUpper(base[:1])+base[1:]] {
					src.names[strings.ToUpper(base[:1])+base[1:]] = true
				}
				continue
			}
			recv, ok := fn.Recv.List[0].Type.(*ast.Ident)
			if fn.Name.Name != "Valid" || !ok || !src.stringTypes[recv.Name] {
				continue
			}
			accepted, ok := switchCases(fn.Body, constValue)
			if !ok {
				t.Fatalf("%s.Valid() is not a switch over constants or string literals; this test cannot read what it accepts", recv.Name)
			}
			src.valid[recv.Name] = accepted
		}
	}
	return src
}

// switchCases returns the values the first switch in body lists in its case
// clauses, resolving constant names through consts.
func switchCases(body *ast.BlockStmt, consts map[string]string) ([]string, bool) {
	for _, stmt := range body.List {
		sw, ok := stmt.(*ast.SwitchStmt)
		if !ok {
			continue
		}
		var out []string
		for _, clause := range sw.Body.List {
			for _, expr := range clause.(*ast.CaseClause).List {
				switch e := expr.(type) {
				case *ast.Ident:
					v, ok := consts[e.Name]
					if !ok {
						return nil, false
					}
					out = append(out, v)
				case *ast.BasicLit:
					v, err := strconv.Unquote(e.Value)
					if err != nil {
						return nil, false
					}
					out = append(out, v)
				default:
					return nil, false
				}
			}
		}
		return out, true
	}
	return nil, false
}

// validOf adapts a type's Valid method to take the plain string.
func validOf[T interface {
	~string
	Valid() bool
}]() func(string) bool {
	return func(s string) bool { return T(s).Valid() }
}

// validators is every vocabulary type with a Valid method. TestVocabularies
// fails if a type has one and is not listed here.
var validators = map[string]func(string) bool{
	"ActivityKind":             validOf[ActivityKind](),
	"AgentTier":                validOf[AgentTier](),
	"ApprovalState":            validOf[ApprovalState](),
	"AudienceKind":             validOf[AudienceKind](),
	"Capability":               validOf[Capability](),
	"ChannelPermission":        validOf[ChannelPermission](),
	"CredentialScope":          validOf[CredentialScope](),
	"DiagnosticEventKind":      validOf[DiagnosticEventKind](),
	"DiagnosticSocket":         validOf[DiagnosticSocket](),
	"DiagnosticVoiceStateName": validOf[DiagnosticVoiceStateName](),
	"GuestEndReason":           validOf[GuestEndReason](),
	"GuestOrigin":              validOf[GuestOrigin](),
	"GuestState":               validOf[GuestState](),
	"HealthVoice":              validOf[HealthVoice](),
	"IdentityClass":            validOf[IdentityClass](),
	"InviteJoinMode":           validOf[InviteJoinMode](),
	"JoinWarning":              validOf[JoinWarning](),
	"LoginNext":                validOf[LoginNext](),
	"OpenJoiningMode":          validOf[OpenJoiningMode](),
	"ProblemReportOften":       validOf[ProblemReportOften](),
	"ProblemReportState":       validOf[ProblemReportState](),
	"SignInLinkPurpose":        validOf[SignInLinkPurpose](),
	"SocketCloseReason":        validOf[SocketCloseReason](),
	"InterventionKind":         validOf[InterventionKind](),
	"InviteKind":               validOf[InviteKind](),
	"InviteeDepth":             validOf[InviteeDepth](),
	"InterventionState":        validOf[InterventionState](),
	"NetRole":                  validOf[NetRole](),
	"OptionKind":               validOf[OptionKind](),
	"PrincipalRole":            validOf[PrincipalRole](),
	"PrincipalState":           validOf[PrincipalState](),
	"Outcome":                  validOf[Outcome](),
	"RunState":                 validOf[RunState](),
	"ServerMode":               validOf[ServerMode](),
	"SessionState":             validOf[SessionState](),
	"SessionStatus":            validOf[SessionStatus](),
	"VoiceControlBlock":        validOf[VoiceControlBlock](),
	"VoiceControlCapture":      validOf[VoiceControlCapture](),
	"VoiceControlCommandKind":  validOf[VoiceControlCommandKind](),
	"VoiceControlCredential":   validOf[VoiceControlCredential](),
	"VoiceControlErrorCode":    validOf[VoiceControlErrorCode](),
	"VoiceControlEventKind":    validOf[VoiceControlEventKind](),
	"VoiceControlPhase":        validOf[VoiceControlPhase](),
	"VoiceControlPlayback":     validOf[VoiceControlPlayback](),
	"VoiceControlPTT":          validOf[VoiceControlPTT](),
	"VoiceControlPTTProblem":   validOf[VoiceControlPTTProblem](),
	"VoiceControlReason":       validOf[VoiceControlReason](),
	"VoiceControlTalkMode":     validOf[VoiceControlTalkMode](),
	"VoiceDevicesConfig":       validOf[VoiceDevicesConfig](),
	"VoiceDevicesPipeWire":     validOf[VoiceDevicesPipeWire](),
	"VoiceShareEndReason":      validOf[VoiceShareEndReason](),
	"VoiceTransmitState":       validOf[VoiceTransmitState](),
}

// namers is every …Names() func, by the type it names. TestVocabularies fails
// if the package has one that is not listed here.
var namers = map[string]func() string{
	"ActivityKind":      activityKindNames,
	"ChannelPermission": channelPermissionNames,
	"InterventionKind":  interventionKindNames,
	"InterventionState": interventionStateNames,
	"RunState":          runStateNames,
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

func TestVocabularies(t *testing.T) {
	src := readSource(t)

	t.Run("every string type has a vocabulary", func(t *testing.T) {
		if got, want := sortedKeys(Vocabularies), sortedKeys(src.stringTypes); !reflect.DeepEqual(got, want) {
			t.Errorf("Vocabularies has %v,\nthe package declares the string types %v", got, want)
		}
	})

	for _, name := range sortedKeys(Vocabularies) {
		table := Vocabularies[name]
		t.Run(name, func(t *testing.T) {
			if got := src.consts[name]; !reflect.DeepEqual(table, got) {
				t.Errorf("Vocabularies[%q] = %q,\nthe package declares the constants %q (in this order)", name, table, got)
			}
			if accepted, ok := src.valid[name]; ok {
				if got, want := slices.Sorted(slices.Values(accepted)), slices.Sorted(slices.Values(table)); !reflect.DeepEqual(got, want) {
					t.Errorf("%s.Valid() accepts %q,\nVocabularies[%q] lists %q", name, got, name, want)
				}
			}
			if valid, ok := validators[name]; ok {
				for _, v := range table {
					if !valid(v) {
						t.Errorf("%s(%q).Valid() = false, want true", name, v)
					}
				}
				if valid("") {
					t.Errorf("%s(\"\").Valid() = true, want false", name)
				}
			}
			if names, ok := namers[name]; ok {
				if got, want := names(), strings.Join(table, ", "); got != want {
					t.Errorf("the %s names are %q, want %q", name, got, want)
				}
			}
		})
	}

	t.Run("every Valid method is called", func(t *testing.T) {
		if got, want := sortedKeys(validators), sortedKeys(src.valid); !reflect.DeepEqual(got, want) {
			t.Errorf("validators has %v,\nthe package has Valid() on %v", got, want)
		}
	})

	t.Run("every Names func is checked", func(t *testing.T) {
		if got, want := sortedKeys(namers), sortedKeys(src.names); !reflect.DeepEqual(got, want) {
			t.Errorf("namers has %v,\nthe package has Names funcs for %v", got, want)
		}
	})
}
