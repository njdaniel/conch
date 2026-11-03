package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The coverage rule for the permission surface (issue #1017, ADR-014, design
// §7.7 item 2). Two things must have a row of the restricted operator or the
// restricted member:
//
//   - every route of class accessPermission: the restricted phase asks each
//     one (permissionRoutes, restricted.go), and permissionRouteCount makes
//     sure the reader of routes.go has not missed one;
//   - every handler check of design §7.3: the gates the server's own honesty
//     test lists (permissionGates, internal/server/permissions_test.go). One
//     list: it is read from that file, not copied here.
//
// A gate's row is named in gateRows by a phrase that must be in the source of
// a phase file; a gate with no row yet is in pendingGates with what is
// waiting for it. Both maps are checked both ways: a gate with neither fails,
// and so does an entry for a gate the list no longer has, or a pending gate
// that has a row.

// rowSources are the files whose text holds the phases' rows.
var rowSources = []string{"restricted.go", "preset.go"}

// gateRows names, for each gate with a restricted row, a phrase of the
// phase that runs it.
var gateRows = map[string]string{
	"routes.go Server.routeTable":                      "the restricted operator on a permission route: ",
	"invites.go Server.handleListInvites":              "the restricted operator's invite list",
	"sessions.go Server.handleListSessions":            "the restricted operator sees the session's runtime",
	"store/invites.go makerRightsTx":                   "the restricted operator: an open link",
	"store/invites.go Store.CloseInvite":               "R closes A's open link",
	"store/sign_in_links.go makerMayActAsTx":           "the restricted operator's sign-in link after the cut",
	"auth.go Server.guard":                             "on A's channel = %d %s, want 404",
	"members.go Server.handleListChannelMembers":       "GET /v1/channels/{channel}/members",
	"nets.go Server.handleListNets":                    "GET /v1/channels/{channel}/nets",
	"store/members.go memberActorStandingTx":           "in its own channel",
	"store/channels_rename.go channelRenameStandingTx": "PUT /v1/channels/{id}/name",
	"store/guest_policy.go checkOpenChannelsWriterTx":  "the joining settings naming A's channel",
	"hooks.go Server.handleCreateHook":                 "a hook into A's channel",
	"manifests.go Server.handlePutManifest":            "a manifest granting A's channel",
	"store/members.go channelCreatePermission":         " channel under personal",
}

// pendingGates are the gates that have no restricted row yet, each with what
// will give it one. The rule fails when a gate is in neither map; when a row
// lands, the gate moves to gateRows, and the rule fails until it does.
var pendingGates = map[string]string{ // #nosec G101 -- gate names, not credentials
	"principals.go Server.handleRenamePrincipal":     "#1190: the rename route's manage_members check",
	"disable.go Server.handleRevokeAllCredentials":   "#1190: the revoke-all route's manage_members check",
	"hooks.go Server.handleListHooks":                "#1190: the other_hooks name filter",
	"auth.go Server.channelSightFor":                 "#1190: the channel names an operator answer may show",
	"join.go Server.joinSettingsResponse":            "#1190: the other_open_channels name filter",
	"store/guest_policy.go admittingOpenChannelsTx":  "#1190: the address join's per-channel re-check",
	"channels.go Server.handleChannelSettings":       "#1190: the channel-settings route's restricted row",
	"store/queries.go Store.ChannelSettings":         "#1190: the channel-settings route's restricted row",
	"store/members.go channelCreatorStandingTx":      "#1190: a creator without manage_channels, bound in its own channel",
	"directory.go Server.handleListDirectoryMembers": "#988: the directory rows (placeholder in preset.go)",
	"voice.go Server.voicePresenceChannel":           "#1190: a member without use_voice at the voice routes",
	"voice.go Server.voiceStillEntitled":             "#1190: a member without use_voice at the voice routes",
	"voice_occupancy.go Server.voiceOccupancyCaller": "#1190: a member without use_voice at the voice routes",
	"voice_presence.go Server.voiceEntitlement":      "#1190: a member without use_voice at the voice routes",
	"voice_presence.go Server.voiceWithheldAmong":    "#1190: a member without use_voice at the voice routes",
	"voice_share.go Server.handleVoiceShareStart":    "#1190: a member without share_screen at the share routes",
	"voice_share.go voicePoller.shareEntitled":       "#1190: a member without share_screen at the share routes",
	"voice_share.go voicePoller.sharesWithheld":      "#1190: a member without share_screen at the share routes",
}

// gateKeys reads the gates of permissionGates out of the server's test file,
// as "file function" keys. An element that is not a composite of exactly two
// string literals is an error naming the permission and the element's index,
// never skipped: a gate the reader cannot see would go uncovered.
func gateKeys(repo string) ([]string, error) {
	path := filepath.Join(repo, "internal", "server", "permissions_test.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil, err
	}
	return gateKeysOf(file)
}

// gateKeysOf reads the permissionGates of a parsed file.
func gateKeysOf(file *ast.File) ([]string, error) {
	var lit *ast.CompositeLit
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "permissionGates" || len(spec.Values) != 1 {
			return true
		}
		lit, _ = spec.Values[0].(*ast.CompositeLit)
		return false
	})
	if lit == nil {
		return nil, fmt.Errorf("internal/server/permissions_test.go has no permissionGates this check can read")
	}
	seen := map[string]bool{}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			return nil, fmt.Errorf("permissionGates: an entry that is not key: value")
		}
		perm := types.ExprString(kv.Key)
		gates, ok := kv.Value.(*ast.CompositeLit)
		if !ok {
			return nil, fmt.Errorf("permissionGates[%s]: the value is not a list of gates", perm)
		}
		for i, g := range gates.Elts {
			pair, ok := g.(*ast.CompositeLit)
			if !ok || len(pair.Elts) != 2 {
				return nil, fmt.Errorf("permissionGates[%s] element %d is not a composite of exactly two string literals", perm, i)
			}
			var parts [2]string
			for j, e := range pair.Elts {
				b, ok := e.(*ast.BasicLit)
				if !ok || b.Kind != token.STRING {
					return nil, fmt.Errorf("permissionGates[%s] element %d is not a composite of exactly two string literals", perm, i)
				}
				s, err := strconv.Unquote(b.Value)
				if err != nil {
					return nil, fmt.Errorf("permissionGates[%s] element %d: %w", perm, i, err)
				}
				parts[j] = s
			}
			seen[parts[0]+" "+parts[1]] = true
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("permissionGates is empty")
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// gateCoverage compares the gates with the rows. missing: a gate with a row
// in neither map. noRow: a gateRows phrase that no string literal of the
// phase sources holds. stale: an entry for a gate the list lacks, or a gate
// in both maps.
func gateCoverage(gates []string, rows, pending map[string]string, literals []string) (missing, noRow, stale []string) {
	held := func(phrase string) bool {
		for _, l := range literals {
			if strings.Contains(l, phrase) {
				return true
			}
		}
		return false
	}
	in := map[string]bool{}
	for _, g := range gates {
		in[g] = true
		_, hasRow := rows[g]
		_, isPending := pending[g]
		if !hasRow && !isPending {
			missing = append(missing, g)
		}
		if hasRow && isPending {
			stale = append(stale, g+" (in both maps)")
		}
	}
	for g, phrase := range rows {
		if !in[g] {
			stale = append(stale, g)
		} else if !held(phrase) {
			noRow = append(noRow, g+" -> "+phrase)
		}
	}
	for g := range pending {
		if !in[g] {
			stale = append(stale, g)
		}
	}
	sort.Strings(missing)
	sort.Strings(noRow)
	sort.Strings(stale)
	return missing, noRow, stale
}

// permissionRouteClass finds every route of class accessPermission, whether
// or not the reader of routes.go understood it.
var permissionRouteClass = regexp.MustCompile(`rt\("([^"]+)", accessPermission,`)

// permissionRouteCount compares the routes of class accessPermission with
// those permissionRoutes read: a route it did not read has no restricted row.
func permissionRouteCount(repo string) (unread []string, err error) {
	src, err := os.ReadFile(filepath.Join(repo, "internal", "server", "routes.go")) // #nosec G304 -- the repository this run is in
	if err != nil {
		return nil, err
	}
	read, err := permissionRoutes(repo)
	if err != nil {
		return nil, err
	}
	for _, m := range permissionRouteClass.FindAllStringSubmatch(string(src), -1) {
		if _, ok := read[m[1]]; !ok {
			unread = append(unread, m[1])
		}
	}
	sort.Strings(unread)
	return unread, nil
}

// rowLiterals is every string literal of the phase files, unquoted. A phrase
// is held only by a literal: a comment, an identifier or a function name that
// happens to carry the words is not a row. gaterows.go itself is never a
// source, or its own phrases would hold themselves.
func rowLiterals(repo string) ([]string, error) {
	var out []string
	for _, f := range rowSources {
		if f == "gaterows.go" {
			return nil, fmt.Errorf("rowSources lists gaterows.go, whose own phrases would hold themselves")
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repo, "e2e", "isolation", f), nil, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, literalsOf(file)...)
	}
	return out, nil
}

// literalsOf is the unquoted string literals of a file, and nothing else.
func literalsOf(file *ast.File) []string {
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		if b, ok := n.(*ast.BasicLit); ok && b.Kind == token.STRING {
			if s, err := strconv.Unquote(b.Value); err == nil {
				out = append(out, s)
			}
		}
		return true
	})
	return out
}
