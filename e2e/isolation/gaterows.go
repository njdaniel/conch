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
var rowSources = []string{"restricted.go", "preset.go", "hiderows.go", "voicegates.go"}

// gateRows names, for each gate with a restricted row, a phrase of the
// phase that runs it.
var gateRows = map[string]string{
	"routes.go Server.routeTable":                      "the restricted operator on a permission route: ",
	"invites.go Server.handleListInvites":              "the restricted operator's invite list",
	"sessions.go Server.handleListSessions":            "the restricted operator sees the session's runtime",
	"store/invites.go makerRightsTx":                   "the restricted operator: a link landing in A's channel",
	"store/invites.go Store.CloseInvite":               "R closes A's open link",
	"store/sign_in_links.go makerMayActAsTx":           "the restricted operator's sign-in link after the cut",
	"auth.go Server.guard":                             "on A's channel = %d %s, want 404",
	"members.go Server.channelMembersGate":             "GET /v1/channels/{channel}/members",
	"nets.go Server.handleListNets":                    "GET /v1/channels/{channel}/nets",
	"store/members.go memberActorStandingTx":           "in its own channel",
	"store/channels_rename.go channelRenameStandingTx": "PUT /v1/channels/{id}/name",
	"store/guest_policy.go checkOpenChannelsWriterTx":  "the joining settings naming A's channel",
	"hooks.go Server.handleCreateHook":                 "a hook into A's channel",
	"manifests.go Server.handlePutManifest":            "a manifest granting A's channel",
	"store/members.go channelCreatePermission":         " channel under personal",

	// Issue #1190: the gates #1017 left pending.
	"principals.go Server.handleRenamePrincipal":     "the restricted operator renames a member",
	"disable.go Server.handleRevokeAllCredentials":   "the restricted operator revokes every credential of a member",
	"hooks.go Server.handleListHooks":                "the restricted operator's hook list",
	"auth.go Server.channelSightFor":                 "R's link of two channels is listed with the one R is in",
	"join.go Server.joinSettingsResponse":            "the joining settings R may read name the default channels it is in",
	"store/guest_policy.go admittingOpenChannelsTx":  "an address join after its lister lost manage_channels",
	"channels.go Server.handleChannelSettings":       "the restricted operator changes a channel's settings",
	"store/members.go channelCreatorStandingTx":      "the restricted operator's channel creation is bounded",
	"directory.go Server.handleListDirectoryMembers": "the directory of a member without see_member_directory",
	"voice.go Server.voicePresenceChannel":           "a member without use_voice at the voice session",
	"voice_occupancy.go Server.voiceOccupancyCaller": "a member without use_voice at voice occupancy",
	"voice_share.go Server.handleVoiceShareStart":    "a member without share_screen at the share claim",

	// Issue #1217: the live-media voice gates (voicegates.go; with LiveKit).
	"voice_presence.go Server.voiceEntitlement": "a participant on a token from before the switch (voiceEntitlement)",
	"voice_share.go voicePoller.sharesWithheld": "a share whose share_screen was cut in the store (sharesWithheld)",

	// Issue #1241: the camera.
	"voice_camera.go Server.handleVoiceCameraStart": "a member without use_camera at the camera claim",
	"voice_camera.go Server.handleVoiceCameraStop":  "the restricted operator stops a member's camera in its own channel",
}

// pendingGates are the gates that have no restricted row yet, each with why
// and what will give it one. The rule fails when a gate is in neither map;
// when a row lands, the gate moves to gateRows, and the rule fails until it
// does. Each of these is reachable only by a race or by a live media
// participant, which a request-and-response row cannot make.
var pendingGates = map[string]string{ // #nosec G101 -- gate names, not credentials
	"store/queries.go Store.ChannelSettings":      "reached only by a race with a cut after handleChannelSettings' own check: no request in the isolation world lands a cut between the handler's check and the store's read; covered by store/channels_v1_test.go (#1217)",
	"voice.go Server.voiceStillEntitled":          "reached only by a cut landing between the handler's check and the token's signing: a race the isolation world has no seam for; covered by server voice_permission_test.go TestVoicePermissionFlipUnderRequests (#1217)",
	"voice_presence.go Server.voiceWithheldAmong": "reached only by a use_voice cut from a column through PUT /v1/permissions (voiceWithheld) or by a role change (voiceWithheldFor); the per-person voice switch takes another path (voiceAfterVoiceOff, voiceEvictLost), which voicegates.go runs, and a column cut would close B's open voice sockets, which the world keeps to its end; covered by server voice_share_revoke_test.go revocations case \"permissions PUT/use_voice\" and voice_camera_revoke_test.go (#1217)",
	"voice_share.go voicePoller.shareEntitled":    "the claim's re-read under the room's claims, reached only by a cut landing inside a claim after the handler's own share_screen check (every later claim is refused by the handler first, voicegates.go): a race the isolation world has no seam for; covered by server voice_share_revoke_test.go TestVoiceShareClaimRevokedBeforeGrant (#1217)",
	// The camera's (issue #1241). Each ends or refuses a live claim, which a
	// request-and-response row cannot hold; the media half's camera rows
	// (media.go) run the PUT's hook against a real one, and the server's
	// tests the rest (voice_camera_revoke_test.go, voice_camera_pass_test.go).
	"voice_camera.go voicePoller.cameraEntitled":  "the claim's re-read under the room's claims: needs a claim held at the lock (server voice_camera_revoke_test.go TestVoiceCameraClaimRevokedBeforeGrant); #1217: a seam-driven race row",
	"voice_camera.go voicePoller.camerasWithheld": "ends a live camera on the pass when use_camera was cut without the hook: needs a live claim and a store edit (server voice_camera_pass_test.go TestVoiceCameraEntitlementEveryPass); #1217: a live-media isolation row",
	"permissions.go Server.endWithheldCameras":    "the PUT's hook ending a live camera when use_camera is cut: run against a real claim in the media half (media.go, cameraA); the phrase rule reads the request phases only",
	"permissions.go Server.endCameraIfWithheld":   "a role change's hook ending a live camera: needs a live claim (server voice_camera_revoke_test.go TestVoiceCameraHooksEndBeforeAnswer); #1217: a live-media isolation row",
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
