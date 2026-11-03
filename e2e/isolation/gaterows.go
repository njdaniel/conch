package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
	"routes.go Server.routeTable":                      "permissionRoutes(",
	"invites.go Server.handleListInvites":              "the restricted operator's invite list",
	"sessions.go Server.handleListSessions":            "the restricted operator sees the session's runtime",
	"store/invites.go makerRightsTx":                   "the restricted operator: an open link",
	"store/invites.go Store.CloseInvite":               "R closes A's open link",
	"store/sign_in_links.go makerMayActAsTx":           "the restricted operator's sign-in link after the cut",
	"auth.go Server.guard":                             "on A's channel = %d %s, want 404",
	"members.go Server.handleListChannelMembers":       `"GET /v1/channels/{channel}/members"`,
	"nets.go Server.handleListNets":                    `"GET /v1/channels/{channel}/nets"`,
	"store/members.go memberActorStandingTx":           "in its own channel",
	"store/channels_rename.go channelRenameStandingTx": `"PUT /v1/channels/{id}/name"`,
	"store/guest_policy.go checkOpenChannelsWriterTx":  "the joining settings naming A's channel",
	"hooks.go Server.handleCreateHook":                 "a hook into A's channel",
	"manifests.go Server.handlePutManifest":            "a manifest granting A's channel",
	"store/members.go channelCreatePermission":         "a member creates a private channel",
}

// pendingGates are the gates that have no restricted row yet, each with what
// will give it one. The rule fails when a gate is in neither map; when a row
// lands, the gate moves to gateRows, and the rule fails until it does.
var pendingGates = map[string]string{
	"principals.go Server.handleRenamePrincipal":     "no restricted row: the rename route's manage_members check (file an issue)",
	"disable.go Server.handleRevokeAllCredentials":   "no restricted row: the revoke-all route's manage_members check (file an issue)",
	"hooks.go Server.handleListHooks":                "no restricted row: the other_hooks name filter (file an issue)",
	"auth.go Server.channelSightFor":                 "no restricted row: the channel names an operator answer may show (file an issue)",
	"join.go Server.joinSettingsResponse":            "no restricted row: the other_open_channels name filter (file an issue)",
	"store/guest_policy.go admittingOpenChannelsTx":  "no restricted row: the address join's per-channel re-check (file an issue)",
	"channels.go Server.handleChannelSettings":       "#981: the channel-settings route's restricted row",
	"store/queries.go Store.ChannelSettings":         "#981: the channel-settings route's restricted row",
	"store/members.go channelCreatorStandingTx":      "#981: a creator without manage_channels, bound in its own channel",
	"directory.go Server.handleListDirectoryMembers": "#988: the directory rows (placeholder in preset.go)",
	"voice.go Server.voicePresenceChannel":           "#984 follow-up: a member without use_voice at the voice routes (file an issue)",
	"voice.go Server.voiceStillEntitled":             "#984 follow-up: a member without use_voice at the voice routes (file an issue)",
	"voice_occupancy.go Server.voiceOccupancyCaller": "#984 follow-up: a member without use_voice at the voice routes (file an issue)",
	"voice_presence.go Server.voiceEntitlement":      "#984 follow-up: a member without use_voice at the voice routes (file an issue)",
	"voice_presence.go Server.voiceWithheldAmong":    "#984 follow-up: a member without use_voice at the voice routes (file an issue)",
	"voice_share.go Server.handleVoiceShareStart":    "#984 follow-up: a member without share_screen at the share routes (file an issue)",
	"voice_share.go voicePoller.shareEntitled":       "#984 follow-up: a member without share_screen at the share routes (file an issue)",
	"voice_share.go voicePoller.sharesWithheld":      "#984 follow-up: a member without share_screen at the share routes (file an issue)",
}

// gateKeys reads the gates of permissionGates out of the server's test file,
// as "file function" keys.
func gateKeys(repo string) ([]string, error) {
	path := filepath.Join(repo, "internal", "server", "permissions_test.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "permissionGates" || len(spec.Values) != 1 {
			return true
		}
		lit, ok := spec.Values[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		found = true
		ast.Inspect(lit, func(m ast.Node) bool {
			pair, ok := m.(*ast.CompositeLit)
			if !ok || len(pair.Elts) != 2 {
				return true
			}
			a, aok := pair.Elts[0].(*ast.BasicLit)
			b, bok := pair.Elts[1].(*ast.BasicLit)
			if !aok || !bok || a.Kind != token.STRING || b.Kind != token.STRING {
				return true
			}
			fileName, err1 := strconv.Unquote(a.Value)
			fn, err2 := strconv.Unquote(b.Value)
			if err1 == nil && err2 == nil {
				seen[fileName+" "+fn] = true
			}
			return false
		})
		return false
	})
	if !found || len(seen) == 0 {
		return nil, fmt.Errorf("internal/server/permissions_test.go has no permissionGates this check can read")
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// gateCoverage compares the gates with the rows. missing: a gate with a row
// in neither map. noRow: a gateRows phrase that no phase source holds.
// stale: an entry for a gate the list lacks, or a gate in both maps.
func gateCoverage(gates []string, rows, pending map[string]string, source string) (missing, noRow, stale []string) {
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
		} else if !strings.Contains(source, phrase) {
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

// rowSource is the text of the phase files.
func rowSource(repo string) (string, error) {
	var b strings.Builder
	for _, f := range rowSources {
		src, err := os.ReadFile(filepath.Join(repo, "e2e", "isolation", f)) // #nosec G304 -- the repository this run is in
		if err != nil {
			return "", err
		}
		b.Write(src)
	}
	return b.String(), nil
}
