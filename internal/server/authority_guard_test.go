package server

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// TestActorAuthorityReadsPermissions is the structural half of ADR-014's
// "one helper" rule (docs/design/admin-roles.md §7.1, N4). What an actor
// may do is decided by a permission (can in the handlers, permittedTx in
// the store), never by the actor's role. It parses internal/server and
// internal/server/store with go/ast and flags, in non-test code:
//
//   - a comparison (== or !=) with RoleOperator or RoleMember;
//   - a switch whose case names RoleOperator or RoleMember;
//   - a call to isOperator;
//   - a string literal holding SQL that compares role with 'operator' or
//     'member'.
//
// Each flagged site must be listed in authorityAllowlist, by file and
// function, with how many sites of that kind it holds and why. A listed
// entry that no longer matches what the code holds fails too, so the list
// cannot go stale. Net roles (schema.NetRole*) and the permission
// document's columns (schema.PermissionRole*) are other words and are not
// flagged.
func TestActorAuthorityReadsPermissions(t *testing.T) {
	hits := scanAuthorityTree(t)
	unlisted, stale := authorityViolations(hits, authorityAllowlist)
	for _, u := range unlisted {
		t.Errorf("actor authority read from the role, not a permission: %s. Ask can() (handlers) or permittedTx (store), or, if this is target protection, sign-in strength or a fixed operator read (design §4.2), list it in authorityAllowlist with its reason.", u)
	}
	for _, s := range stale {
		t.Errorf("authorityAllowlist entry no longer matches the code: %s. Remove or correct it.", s)
	}
}

// TestEveryPermissionIsChecked is the "no dead permission" rule (design
// §7.1): every schema.Permission is named by at least one check in
// internal/server or internal/server/store, outside the permissions helper
// itself, or is listed in notYetGated with the issue that gates it. A
// notYetGated entry that is checked somewhere fails, so the list shrinks as
// the gates land.
func TestEveryPermissionIsChecked(t *testing.T) {
	if len(permissionConsts) != len(schema.Permissions) {
		t.Fatalf("permissionConsts has %d entries, schema.Permissions %d: keep them in step", len(permissionConsts), len(schema.Permissions))
	}
	for _, p := range schema.Permissions {
		if !slices.Contains(permissionValues(), p) {
			t.Fatalf("permissionConsts lacks %s", p)
		}
	}
	named := scanPermissionNames(t)
	for name, p := range permissionConsts {
		reason, waiting := notYetGated[p]
		switch {
		case named[name] && waiting:
			t.Errorf("%s is checked now (schema.%s) but is still in notYetGated (%s): remove it", p, name, reason)
		case !named[name] && !waiting:
			t.Errorf("%s is named by no check in internal/server: gate it, or list it in notYetGated with the issue that will", p)
		}
	}
}

// notYetGated is each permission no route or store check reads yet, with
// the issue that wires its gate (ADR-014 design §12). The member cell of
// row 4 gates a route that does not exist yet; row 5 is A4's to wire.
// Rows 1 and 2 left with #981 (member channel creation), and rows 10 and 11
// with A3b (#1015), whose route classes read them.
var notYetGated = map[schema.Permission]string{
	schema.PermSeeMemberDirectory: "#983, the member directory",
	schema.PermUseVoice:           "A4, the member permissions on voice",
}

// permissionConsts names each schema.Permission constant, so the scan can
// tell which are named in code. TestEveryPermissionIsChecked keeps it in
// step with schema.Permissions.
var permissionConsts = map[string]schema.Permission{
	"PermCreatePublicChannels":  schema.PermCreatePublicChannels,
	"PermCreatePrivateChannels": schema.PermCreatePrivateChannels,
	"PermMakeMemberLinks":       schema.PermMakeMemberLinks,
	"PermSeeMemberDirectory":    schema.PermSeeMemberDirectory,
	"PermUseVoice":              schema.PermUseVoice,
	"PermShareScreen":           schema.PermShareScreen,
	"PermManageMembers":         schema.PermManageMembers,
	"PermActAsMembers":          schema.PermActAsMembers,
	"PermManageChannels":        schema.PermManageChannels,
	"PermChangeServerSettings":  schema.PermChangeServerSettings,
	"PermReadAuditLog":          schema.PermReadAuditLog,
}

func permissionValues() []schema.Permission {
	m := permissionConsts
	out := make([]schema.Permission, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// authorityKind is one of the patterns the scan flags.
type authorityKind string

const (
	kindRoleCompare authorityKind = "role-compare"
	kindRoleSwitch  authorityKind = "role-switch"
	kindIsOperator  authorityKind = "isOperator"
	kindRoleHelper  authorityKind = "role-helper"
	kindRoleSQL     authorityKind = "role-sql"
)

// authorityHit is one flagged site.
type authorityHit struct {
	file string // relative to internal/server, slash-separated
	fn   string // "Name", "Recv.Name", or "<package>" outside any function
	kind authorityKind
	pos  string
}

// authorityAllowed is one allowlisted function: how many sites of kind it
// holds, and why each is not an actor-authority decision.
type authorityAllowed struct {
	file, fn string
	kind     authorityKind
	count    int
	reason   string
}

// The reasons, as design §4.2 and the issue (#1014) name them.
const (
	whyTarget      = "target protection (design §4.2): who is protected, by the target's role or owner mark, not who may act"
	whySignIn      = "sign-in strength (design §4.2): an account is as strong as the most power it could hold"
	whyIdentity    = "whoami's identity class (design §4.2): what the caller is, not what it may do"
	whyPermit      = "the conversion permits keep the role check until #1005 retires them (N3)"
	whyFixedOp     = "the fixed operator rights (design §4.2): the route guard's accessOperator class, which A3b (#1015) left on the reports, the settings reads and reading the permissions document"
	whyColumn      = "a change's column of the permissions document (schema.PermissionRole), not a principal's role: the operator column is owner-only (§4.3)"
	whyHelper      = "the permissions helper itself: it maps a role onto its document column"
	whyRoleAdmin   = "the owner-only role administration (design §4.3): it sets, validates and protects roles, it does not read the actor's"
	whyOwnerMark   = "the owner mark: an owner is an operator with owner = 1 (ADR-010), fixed, not a permission"
	whyFilter      = "a ?role= list filter: it names a role to match, and decides nothing"
	whyDefinition  = "isOperator's own definition; its calls are flagged one by one"
	whySchemaCheck = "the schema's CHECK constraints and the migration that marked the first owners"
)

// authorityAllowlist is every site the structural test lets read a role. A
// new actor-role check is not added here: it asks a permission instead.
var authorityAllowlist = []authorityAllowed{
	// internal/server: handlers.
	{"auth.go", "isOperator", kindRoleCompare, 1, whyDefinition},
	{"auth.go", "Server.guard", kindIsOperator, 1, whyFixedOp},
	{"permissions.go", "Server.handlePutPermissions", kindRoleCompare, 1, whyColumn},
	{"permissions.go", "turnedOff", kindRoleCompare, 1, whyColumn},
	{"guests.go", "Server.guestWhoAmI", kindIsOperator, 1, whyIdentity},
	{"invites.go", "bulkDisableTargets", kindRoleCompare, 1, whyTarget + ": a bulk disable never touches an operator"},
	{"login.go", "Server.handleLogin", kindRoleCompare, 3, whySignIn + ": an operator's second factor"},
	{"operator_auth.go", "Server.recentAuthGate", kindIsOperator, 2, whySignIn + ": an operator re-authenticates"},
	{"operator_auth.go", "Server.setupCaller", kindIsOperator, 1, whySignIn + ": the first operator's setup credential"},
	{"password_recovery.go", "Server.authWhoAmI", kindIsOperator, 1, whySignIn},
	{"password_recovery.go", "Server.handleReauth", kindIsOperator, 3, whySignIn + ": an operator re-authenticates with its second factor"},
	{"password_recovery.go", "Server.handleChangePassword", kindIsOperator, 3, whySignIn + ": an operator's password change needs its second factor"},
	{"password_recovery.go", "Server.handleSetPassword", kindIsOperator, 1, whySignIn + ": an operator's password needs its second factor"},
	{"password_recovery.go", "Server.passwordOnlyOperator", kindIsOperator, 1, whySignIn},
	{"principals_admin.go", "principalFilterFrom", kindRoleSwitch, 1, whyFilter},
	{"sign_in_links.go", "Server.deviceLinkCaller", kindIsOperator, 1, whySignIn + ": an operator never signs in by a device link"},

	// internal/server/store.
	{"store/credentials.go", "Store.RotateCredential", kindRoleCompare, 1, whyTarget + ": an owner's credential"},
	{"store/credentials.go", "Store.revokeCredential", kindRoleCompare, 2, whyTarget + ": an owner's credential, and the last owner's sign-in"},
	{"store/disable.go", "Store.RevokeAllCredentialsAs", kindRoleCompare, 1, whyTarget + ": the last owner's sign-in"},
	{"store/disable.go", "disablePrincipalTx", kindRoleCompare, 1, whyTarget + ": the last owner"},
	{"store/disable.go", "liveOperatorCredentialsTx", kindRoleSQL, 2, whyTarget + ": counts the owners who can still sign in"},
	{"store/hide.go", "Store.HidePrincipal", kindRoleCompare, 1, whyTarget + ": a bulk hide never touches an operator"},
	{"store/hide.go", "Store.HideTargets", kindRoleSQL, 1, whyTarget + ": a bulk hide never touches an operator"},
	{"store/hooks.go", "Store.ListHooks", kindRoleSQL, 1, whyTarget + ": an owner's hooks are hidden from non-owners"},
	{"store/hooks.go", "Store.RevokeHook", kindRoleCompare, 1, whyTarget + ": an owner's hook"},
	{"store/members.go", "Store.RemoveChannelMember", kindRoleCompare, 1, whyTarget + ": a manager never removes an operator"},
	{"store/members.go", "checkManagerAddTargetTx", kindRoleCompare, 1, whyTarget + ": a manager adds members only"},
	{"store/mode.go", "Store.StartMode", kindRoleSQL, 1, whySignIn},
	{"store/mode.go", "endPasswordOnlyTx", kindRoleSQL, 1, whySignIn},
	{"store/operator_auth.go", "Store.CompleteTOTPLogin", kindRoleCompare, 1, whySignIn},
	{"store/operator_auth.go", "hostOperatorTx", kindRoleCompare, 1, whySignIn},
	{"store/operator_auth.go", "setupCallerTx", kindRoleCompare, 1, whySignIn},
	{"store/passwords.go", "Store.ChangePassword", kindRoleCompare, 2, whySignIn},
	{"store/passwords.go", "Store.EnrollPassword", kindRoleCompare, 1, whySignIn},
	{"store/passwords.go", "Store.recordRecentAuth", kindRoleCompare, 1, whySignIn},
	{"store/permissions.go", "PermissionRoleOf", kindRoleCompare, 1, whyHelper},
	{"store/permissions.go", "principalHolds", kindRoleSwitch, 1, whyHelper},
	{"store/queries.go", "Principal.IsOwner", kindRoleCompare, 1, whyOwnerMark},
	{"store/queries.go", "ownerProtectedTx", kindRoleCompare, 1, whyTarget + ": nobody but an owner acts on an owner"},
	{"store/roles.go", "Store.BootstrapOperatorWith", kindRoleSQL, 1, whyRoleAdmin + ": the first operator"},
	{"store/roles.go", "reportPromotedOwners", kindRoleSQL, 1, whyRoleAdmin + ": the migration's report of owners"},
	{"store/roles.go", "setRoleTx", kindRoleCompare, 7, whyRoleAdmin},
	{"store/hide.go", "Store.ListPrincipals", kindRoleCompare, 1, whyFilter},
	{"store/passwords.go", "recheckLoginTx", kindRoleCompare, 1, whySignIn + ": a login finishes only for the role it began under, so a promotion mid-login cannot skip the operator's second factor"},
	{"store/operator_auth.go", "Store.BeginTOTPEnrollment", kindRoleHelper, 1, whySignIn},
	{"store/operator_auth.go", "Store.ConfirmTOTP", kindRoleHelper, 1, whySignIn},
	{"store/operator_auth.go", "Store.IssueSetupToken", kindRoleHelper, 1, whySignIn},
	{"store/operator_auth.go", "Store.RecoverOperator", kindRoleHelper, 1, whySignIn},
	{"store/operator_auth.go", "Store.RedeemSetupToken", kindRoleHelper, 1, whySignIn},
	{"store/permissions.go", "brakeWithholds", kindRoleHelper, 1, whyHelper + ": the may_invite brake binds the member column only (design §5)"},
	{"store/permissions.go", "putPermissionsTx", kindRoleCompare, 1, whyHelper + ": a change's column, not a principal's role: the operator column is owner-only (§4.3)"},
	{"store/sign_in_links.go", "Store.CreateConversionPermit", kindRoleHelper, 1, whyPermit},
	{"store/sign_in_links.go", "usableConversionPermitTx", kindRoleHelper, 1, whyPermit},
	{"auth.go", "refusedPermission", kindRoleHelper, 1, "a refusal's wording (design §7.4): whether the caller's column could hold the permission it lacks, so a member is never named an operator permission; decides nothing"},
	{"store/sign_in_links.go", "Store.CloseSignInLink", kindRoleCompare, 1, whyTarget + ": an owner's link"},
	{"store/sign_in_links.go", "makerIsOperatorTx", kindRoleCompare, 1, whyPermit},
	{"store/sign_in_links.go", "signInLinkTargetTx", kindRoleCompare, 1, whySignIn + ": a sign-in link never targets an operator"},
	{"store/store.go", "<package>", kindRoleSQL, 5, whySchemaCheck},
}

// roleSQL is SQL that compares a principal's role with a role literal.
var roleSQL = regexp.MustCompile(`(?i)\brole\s*(?:=|==|<>|!=)\s*'(?:operator|member)'|\brole\s+(?:not\s+)?in\s*\([^)]*'(?:operator|member)'`)

// roleConsts are the names of every principal-role value a check could
// compare with: the store's and the wire's principal roles, and whoami's
// identity classes. The document's columns (schema.PermissionRole*) are
// not: the permissions helper compares them to read the document, and a
// principal reaches a column only through PermissionRoleOf, whose callers
// are counted as roleHelpers.
var roleConsts = map[string]bool{
	"RoleOperator": true, "RoleMember": true,
	"PrincipalRoleOperator": true, "PrincipalRoleMember": true,
	"IdentityOperator": true, "IdentityMember": true,
}

// roleHelpers are the allowlisted functions that answer a role question
// themselves: a new call to one is a new role-based decision, counted as
// isOperator calls are.
var roleHelpers = map[string]bool{
	"isOperator": true, "makerIsOperatorTx": true, "hostOperatorTx": true, "setupCallerTx": true,
	"PermissionRoleOf": true,
}

// conversions are the conversions a role passes through on its way to a
// comparison: Role(role), string(p.Role), schema.PrincipalRole(p.Role).
var conversions = map[string]bool{"Role": true, "string": true, "PrincipalRole": true, "PermissionRole": true}

// unwrapRole strips parentheses and role conversions from e.
func unwrapRole(e ast.Expr) ast.Expr {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
			continue
		case *ast.CallExpr:
			if len(x.Args) == 1 && conversions[calleeName(x.Fun)] {
				e = x.Args[0]
				continue
			}
		}
		return e
	}
}

// calleeName is the name a call is made by: f for f(x), F for pkg.F(x) or
// recv.F(x).
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// isRoleName reports whether e is a role value: one of roleConsts, or the
// literal "operator" or "member".
func isRoleName(e ast.Expr) bool {
	switch x := unwrapRole(e).(type) {
	case *ast.Ident:
		return roleConsts[x.Name]
	case *ast.SelectorExpr:
		return roleConsts[x.Sel.Name]
	case *ast.BasicLit:
		if x.Kind != token.STRING || len(x.Value) < 2 {
			return false
		}
		v := x.Value[1 : len(x.Value)-1]
		return v == "operator" || v == "member"
	}
	return false
}

// isRoleField reports whether e is a principal's role field: x.Role.
func isRoleField(e ast.Expr) bool {
	sel, ok := unwrapRole(e).(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Role"
}

// scanAuthority returns the flagged sites in one parsed file; rel is its
// path relative to internal/server.
func scanAuthority(fset *token.FileSet, rel string, file *ast.File) []authorityHit {
	var hits []authorityHit
	add := func(fn string, kind authorityKind, n ast.Node) {
		hits = append(hits, authorityHit{file: rel, fn: fn, kind: kind, pos: fset.Position(n.Pos()).String()})
	}
	visit := func(fn string, root ast.Node) {
		ast.Inspect(root, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BinaryExpr:
				if (x.Op == token.EQL || x.Op == token.NEQ) &&
					(isRoleName(x.X) || isRoleName(x.Y) || isRoleField(x.X) || isRoleField(x.Y)) {
					add(fn, kindRoleCompare, x)
				}
			case *ast.SwitchStmt:
				if x.Tag == nil {
					return true
				}
				if isRoleField(x.Tag) {
					add(fn, kindRoleSwitch, x)
					return true
				}
				for _, stmt := range x.Body.List {
					cc, ok := stmt.(*ast.CaseClause)
					if !ok {
						continue
					}
					if slices.ContainsFunc(cc.List, isRoleName) {
						add(fn, kindRoleSwitch, x)
						break
					}
				}
			case *ast.CallExpr:
				switch name := calleeName(x.Fun); {
				case name == "isOperator":
					add(fn, kindIsOperator, x)
				case roleHelpers[name]:
					add(fn, kindRoleHelper, x)
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING && roleSQL.MatchString(x.Value) {
					add(fn, kindRoleSQL, x)
				}
			}
			return true
		})
	}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			visit("<package>", decl)
			continue
		}
		visit(funcName(fd), fd)
	}
	return hits
}

// funcName is a function as the allowlist names it: "Name", or "Recv.Name"
// for a method, the receiver's pointer dropped.
func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// authorityViolations compares hits with allow: unlisted names each
// function whose sites of a kind are not listed or exceed the listed count;
// stale names each entry that does not match what the code holds.
func authorityViolations(hits []authorityHit, allow []authorityAllowed) (unlisted, stale []string) {
	type key struct {
		file, fn string
		kind     authorityKind
	}
	found := map[key][]authorityHit{}
	for _, h := range hits {
		k := key{h.file, h.fn, h.kind}
		found[k] = append(found[k], h)
	}
	listed := map[key]authorityAllowed{}
	for _, a := range allow {
		listed[key{a.file, a.fn, a.kind}] = a
	}
	for k, hs := range found {
		a, ok := listed[k]
		if ok && a.count == len(hs) && a.reason != "" {
			continue
		}
		where := make([]string, len(hs))
		for i, h := range hs {
			where[i] = h.pos
		}
		if ok {
			stale = append(stale, fmt.Sprintf("%s %s %s: listed %d, found %d (%s)", k.file, k.fn, k.kind, a.count, len(hs), strings.Join(where, ", ")))
			continue
		}
		unlisted = append(unlisted, fmt.Sprintf("%s %s %s at %s", k.file, k.fn, k.kind, strings.Join(where, ", ")))
	}
	for k, a := range listed {
		if _, ok := found[k]; !ok {
			stale = append(stale, fmt.Sprintf("%s %s %s: listed %d, found none", a.file, a.fn, a.kind, a.count))
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	return unlisted, stale
}

// authoritySources walks internal/server and internal/server/store, parsing
// each non-test Go file, and calls fn with its path relative to
// internal/server.
func authoritySources(t *testing.T, fn func(fset *token.FileSet, rel string, file *ast.File)) {
	t.Helper()
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(path)
		if d.IsDir() {
			if rel == "." || rel == "store" {
				return nil
			}
			return fs.SkipDir
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		checked++
		fn(fset, rel, file)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 50 {
		t.Fatalf("only %d source files were checked; the walk is not finding the server and the store", checked)
	}
}

func scanAuthorityTree(t *testing.T) []authorityHit {
	t.Helper()
	var hits []authorityHit
	authoritySources(t, func(fset *token.FileSet, rel string, file *ast.File) {
		hits = append(hits, scanAuthority(fset, rel, file)...)
	})
	return hits
}

// scanPermissionNames is the set of schema.Perm* constants named in the
// server and the store, outside the permissions helper (store/permissions.go,
// which names every permission to implement the document).
func scanPermissionNames(t *testing.T) map[string]bool {
	t.Helper()
	named := map[string]bool{}
	authoritySources(t, func(_ *token.FileSet, rel string, file *ast.File) {
		// The permissions helper, and the document's own routes: they read
		// and list the document (its safety net names every enforced
		// permission), and gate nothing beyond the PUT, whose route names its
		// permission in routes.go.
		if rel == "store/permissions.go" || rel == "permissions.go" {
			return
		}
		for _, decl := range file.Decls {
			// whoami reports what the caller holds; it gates nothing, so a
			// permission it names is not thereby checked anywhere.
			if fd, ok := decl.(*ast.FuncDecl); ok && rel == "auth.go" && funcName(fd) == "Server.handleWhoAmI" {
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "schema" {
					if _, isPerm := permissionConsts[sel.Sel.Name]; isPerm {
						named[sel.Sel.Name] = true
					}
				}
				return true
			})
		}
	})
	return named
}

// TestAuthorityScanCatchesEachPattern proves the scan sees each flagged
// pattern, and nothing in the code that only looks like one: a net role, a
// permission column, a role written as a value.
func TestAuthorityScanCatchesEachPattern(t *testing.T) {
	const src = `package fixture

func compares(p Principal) bool { return p.Role == RoleOperator }
func comparesNot(p Principal) bool { return p.Role != store.RoleMember }
func comparesCast(role string) bool { return Role(role) == RoleOperator }
func switches(p Principal) int {
	switch p.Role {
	case RoleOperator:
		return 1
	}
	return 0
}
func callsIsOperator(p Principal) bool { return isOperator(p) }
func sqlEquals() string { return "SELECT 1 FROM principals WHERE role = 'operator'" }
func sqlNotEquals() string { return ` + "`" + `SELECT 1 FROM principals WHERE p.role <> 'member'` + "`" + ` }
func sqlIn() string { return "SELECT 1 FROM principals WHERE role IN ('operator', 'member')" }

func literal(p Principal) bool { return p.Role == "operator" }
func literalLeft(role string) bool { return "member" != role }
func wireRole(r schema.PrincipalRole) bool { return r == schema.RoleOperator }
func futureWireRole(r schema.PrincipalRole) bool { return r == schema.PrincipalRoleOperator }
func identity(c IdentityClass) bool { return c == schema.IdentityOperator }
func parens(p Principal) bool { return (p.Role) == (RoleOperator) }
func againstVariable(p Principal, role Role) bool { return p.Role != role }
func converted(p Principal) bool { return string(p.Role) == want }
func switchOnField(p Principal) int {
	switch p.Role {
	case wanted:
		return 1
	}
	return 0
}
func switchOnLiteral(role string) int {
	switch role {
	case "member":
		return 1
	}
	return 0
}
func callsMakerIsOperator() { _, _ = makerIsOperatorTx(ctx, tx, 1) }
func callsHostOperator() { _, _ = hostOperatorTx(ctx, tx, 1) }
func callsSetupCaller() { _ = setupCallerTx(ctx, tx, 1, 2, now) }
func callsPermissionRoleOf(p Principal) { _ = store.PermissionRoleOf(p) }

func netRole(r NetRole) bool { return r == schema.NetRoleMember }
func otherLiteral(kind string) bool { return kind == "human" }
func otherField(p Principal) bool { return p.Kind == PrincipalHuman }
func column(r PermissionRole) bool { return r == schema.PermissionRoleOperator }
func value() Principal { return Principal{Role: RoleMember} }
func insert() string { return "INSERT INTO principals (role) VALUES ('member')" }
func taglessSwitch(p Principal) int {
	switch {
	case p.Owner:
		return 1
	}
	return 0
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]authorityKind{}
	for _, h := range scanAuthority(fset, "fixture.go", file) {
		if prev, dup := got[h.fn]; dup && prev != h.kind {
			t.Errorf("%s flagged as both %s and %s", h.fn, prev, h.kind)
		}
		got[h.fn] = h.kind
	}
	want := map[string]authorityKind{
		"compares":        kindRoleCompare,
		"comparesNot":     kindRoleCompare,
		"comparesCast":    kindRoleCompare,
		"switches":        kindRoleSwitch,
		"callsIsOperator": kindIsOperator,
		"sqlEquals":       kindRoleSQL,
		"sqlNotEquals":    kindRoleSQL,
		"sqlIn":           kindRoleSQL,

		"literal":               kindRoleCompare,
		"literalLeft":           kindRoleCompare,
		"wireRole":              kindRoleCompare,
		"futureWireRole":        kindRoleCompare,
		"identity":              kindRoleCompare,
		"parens":                kindRoleCompare,
		"againstVariable":       kindRoleCompare,
		"converted":             kindRoleCompare,
		"switchOnField":         kindRoleSwitch,
		"switchOnLiteral":       kindRoleSwitch,
		"callsMakerIsOperator":  kindRoleHelper,
		"callsHostOperator":     kindRoleHelper,
		"callsSetupCaller":      kindRoleHelper,
		"callsPermissionRoleOf": kindRoleHelper,
	}
	for fn, kind := range want {
		if got[fn] != kind {
			t.Errorf("%s: flagged %q, want %q", fn, got[fn], kind)
		}
	}
	for fn, kind := range got {
		if _, ok := want[fn]; !ok {
			t.Errorf("%s flagged %s, but it reads no actor's role", fn, kind)
		}
	}
}

// TestAuthorityViolationsFailsOnNewAndStale proves the comparison fails on
// a site nobody listed, on a listed count the code exceeds, on an entry
// with no reason, and on an entry that matches nothing.
func TestAuthorityViolationsFailsOnNewAndStale(t *testing.T) {
	hit := func(file, fn string, kind authorityKind) authorityHit {
		return authorityHit{file: file, fn: fn, kind: kind, pos: file + ":1"}
	}
	tests := []struct {
		name          string
		hits          []authorityHit
		allow         []authorityAllowed
		unlisted      int
		stale         int
		wantUnlistedS string
	}{
		{
			name:  "listed exactly",
			hits:  []authorityHit{hit("a.go", "f", kindIsOperator)},
			allow: []authorityAllowed{{"a.go", "f", kindIsOperator, 1, "why"}},
		},
		{
			name:          "a new site",
			hits:          []authorityHit{hit("a.go", "f", kindIsOperator), hit("b.go", "g", kindRoleCompare)},
			allow:         []authorityAllowed{{"a.go", "f", kindIsOperator, 1, "why"}},
			unlisted:      1,
			wantUnlistedS: "b.go g role-compare",
		},
		{
			name:  "one more site in a listed function",
			hits:  []authorityHit{hit("a.go", "f", kindIsOperator), hit("a.go", "f", kindIsOperator)},
			allow: []authorityAllowed{{"a.go", "f", kindIsOperator, 1, "why"}},
			stale: 1,
		},
		{
			name:  "no reason",
			hits:  []authorityHit{hit("a.go", "f", kindIsOperator)},
			allow: []authorityAllowed{{"a.go", "f", kindIsOperator, 1, ""}},
			stale: 1,
		},
		{
			name:  "a stale entry",
			hits:  nil,
			allow: []authorityAllowed{{"a.go", "f", kindIsOperator, 1, "why"}},
			stale: 1,
		},
		{
			name:  "same function, other kind",
			hits:  []authorityHit{hit("a.go", "f", kindRoleSQL)},
			allow: []authorityAllowed{{"a.go", "f", kindIsOperator, 1, "why"}},
			// The SQL site is unlisted and the isOperator entry is stale.
			unlisted: 1,
			stale:    1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unlisted, stale := authorityViolations(tt.hits, tt.allow)
			if len(unlisted) != tt.unlisted || len(stale) != tt.stale {
				t.Fatalf("unlisted %v, stale %v; want %d and %d", unlisted, stale, tt.unlisted, tt.stale)
			}
			if tt.wantUnlistedS != "" && !strings.HasPrefix(unlisted[0], tt.wantUnlistedS) {
				t.Errorf("unlisted[0] = %q, want it to start %q", unlisted[0], tt.wantUnlistedS)
			}
		})
	}
}
