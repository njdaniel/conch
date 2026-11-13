package server

import (
	"go/ast"
	"go/token"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestPrincipalReadsStayBehindTheMayKnowPredicate is the structural half of
// the "written once" rule for who a viewer may know (issue #988, design
// §13.2, ADR-014). Which principals a viewer may know is decided by one SQL
// fragment in store/directory.go (knowsCond, through knownPrincipalsSQL), and
// every read of the principals table that can show a member another
// principal goes through it. The test parses internal/server/store and finds
// every function holding a SQL string that selects from or joins principals;
// each must be listed in principalReads with how many such strings it holds
// and the reason it may. A new query over principals is not listed, so it
// fails until its author says which kind it is, which is the moment to ask
// whether it needs the predicate:
//
//   - readPredicate: the readers of the predicate itself. Each must name the
//     fragment (a function that stops naming it fails).
//   - readGated: rows that are a channel's own members, sessions or authors,
//     or a principal a channel write names, reached only behind the channel
//     gate (404 to a non-member) or the message visibility function. Everyone
//     on such a list shares that channel with the viewer, so the predicate is
//     satisfied by the gate.
//   - readOperatorOrMaker: reached only by operator routes (accessOperator or
//     a held permission), or by the maker of a link reading their own joiners
//     (O14b).
//   - readOwnOrInternal: the caller's own row, identity and credential
//     checks, and bookkeeping that is never served to a member.
//
// A listed entry that no longer matches the code fails too. The listing is
// the real guard: it makes every new query over principals say what it is.
// The tripwire for a second copy of the shared-channel clause outside the
// fragment is only a backstop: it keys on the fragment's aliases (m1, m2), so
// a copy written with other aliases is caught by the listing, not by it.
type principalReadKind int

const (
	readPredicate principalReadKind = iota + 1
	readGated
	readOperatorOrMaker
	readOwnOrInternal
)

type principalRead struct {
	file, fn string
	n        int
	kind     principalReadKind
}

var principalReads = []principalRead{
	// #1330: the admin door's target name, read only after the caller is
	// found to hold the catalog permission (an operator's, or an AI
	// administrator's grant), and at the run by the agent that asked.
	{"store/admin_actions.go", "adminTargetNameTx", 1, readOperatorOrMaker},
	{"store/approvals.go", "Store.castDecision", 2, readGated},
	// #851: the ban list, behind the channel-manager class (an owner, an
	// operator holding manage_channels, or a manager of the channel), and a
	// manager's ban target, a member of the channel it manages.
	{"store/bans.go", "Store.ListChannelBans", 1, readOperatorOrMaker},
	{"store/bans.go", "managerBanTargetTx", 1, readOperatorOrMaker},
	{"store/audience.go", "Store.ListChannelAuthors", 1, readGated},
	// #1282: the humans who hold a message of an author that ListChannelAuthors
	// would name it for, decided per reader by messageVisible; the ids go only
	// to the hide signal, never to a member.
	{"store/audience.go", "Store.AuthorReaders", 1, readGated},
	// #1713: the reader's own current name, to match "@name" in the feed.
	{"store/audience.go", "Store.ListPendingNotifications", 1, readOwnOrInternal},
	// #1715: the push sender's target query reads each candidate's name (for
	// the mention rule), kind, disabled, hidden and generation, to decide whom
	// to wake. It serves nothing: a wake carries only a registration id.
	{"store/audience.go", "pushCandidatesSQL", 1, readOwnOrInternal},
	// #1715: the check before a retry: the registration's credential is live,
	// which reads its principal's generation. It returns a yes or a no.
	{"store/push_registrations.go", "Store.PushRegistrationLive", 1, readOwnOrInternal},
	// #1714: compares credentials' generations with their principal's, to end
	// the registrations of dead credentials; reads no name and serves no one.
	{"store/push_registrations.go", "endGenerationDeadPushTx", 1, readOwnOrInternal},
	// #1714: the liveness join of a registration's write, over the caller's
	// own credential and its principal's generation; names nothing.
	{"store/push_registrations.go", "Store.CreatePushRegistration", 1, readOwnOrInternal},
	{"store/push_registrations.go", "Store.ReplacePushTarget", 1, readOwnOrInternal},
	// #1713: the sweep compares a credential's generation with its principal's; never served.
	{"store/notifications.go", "sweepNotificationPreferencesTx", 1, readOwnOrInternal},
	// #1202: managers on the ?scope=all listing, for holders of manage_channels.
	{"store/channels_self.go", "Store.channelDirectoryAll", 1, readOperatorOrMaker},
	// #1150: the last-member count, never served to anyone.
	{"store/channels_self.go", "Store.LeaveChannel", 1, readOwnOrInternal},
	{"store/community.go", "isMemberTx", 1, readOwnOrInternal},
	{"store/community.go", "migrateToCommunity", 1, readOwnOrInternal},
	{"store/credentials.go", "Store.ListCredentials", 1, readOwnOrInternal},
	{"store/credentials.go", "Store.ListLiveCredentials", 1, readOwnOrInternal},
	{"store/credentials.go", "Store.RotateCredential", 2, readOwnOrInternal},
	{"store/credentials.go", "Store.createCredential", 1, readOwnOrInternal},
	{"store/credentials.go", "Store.revokeCredential", 1, readOwnOrInternal},
	{"store/credentials.go", "insertCredentialTx", 1, readOwnOrInternal},
	{"store/directory.go", "directoryKnowersTx", 1, readPredicate},
	{"store/directory.go", "Store.DirectoryMembershipAudience", 2, readPredicate},
	{"store/directory.go", "Store.ListDirectory", 1, readPredicate},
	{"store/directory.go", "mayKnowTx", 1, readPredicate},
	{"store/disable.go", "liveOperatorCredentialsTx", 2, readOwnOrInternal},
	{"store/disable.go", "principalState", 1, readOperatorOrMaker},
	{"store/issued.go", "Store.RevokeIssued", 2, readOperatorOrMaker},
	{"store/issued.go", "backfillIssuedBy", 2, readOwnOrInternal},
	{"store/issued.go", "issuedToOthersTx", 2, readOperatorOrMaker},
	{"store/guests.go", "conversionPolicyTx", 1, readOwnOrInternal},
	{"store/guests.go", "liveGuestTx", 1, readOwnOrInternal},
	{"store/guests.go", "reservedGuestNamesTx", 1, readOwnOrInternal},
	{"store/hide.go", "Store.HideTargets", 2, readOperatorOrMaker},
	{"store/hide.go", "adminPrincipalsTx", 1, readOperatorOrMaker},
	{"store/hide.go", "reportReservedNames", 1, readOperatorOrMaker},
	{"store/hooks.go", "Store.CreateHookWithLabel", 1, readOwnOrInternal},
	{"store/hooks.go", "Store.ListHooks", 1, readOwnOrInternal},
	{"store/hooks.go", "Store.RevokeHook", 1, readOwnOrInternal},
	{"store/interventions.go", "Store.CreateIntervention", 1, readGated},
	{"store/invites.go", "Store.CloseInvite", 1, readOperatorOrMaker},
	{"store/invites.go", "Store.CreateInvite", 1, readOperatorOrMaker},
	{"store/invites.go", "Store.InviteJoiners", 1, readOperatorOrMaker},
	{"store/invites.go", "Store.Invitees", 1, readOperatorOrMaker},
	{"store/invites.go", "Store.ListInvites", 1, readOperatorOrMaker},
	{"store/invites.go", "Store.SetMayInvite", 2, readOperatorOrMaker},
	{"store/invites.go", "Store.invitedBy", 1, readOperatorOrMaker},
	{"store/invites.go", "usableInviteTx", 1, readOperatorOrMaker},
	{"store/invites_v2.go", "Store.CreateMemberLink", 1, readOperatorOrMaker},
	{"store/invites_v2.go", "Store.ListMemberLinks", 1, readOperatorOrMaker},
	{"store/invites_v2.go", "inviteJoinersTx", 1, readOperatorOrMaker},
	{"store/manifests.go", "Store.CountAgentsWithoutManifest", 1, readOperatorOrMaker},
	{"store/manifests.go", "Store.PutAgentManifest", 1, readOwnOrInternal},
	{"store/members.go", "Store.AddChannelMemberIfAbsent", 1, readGated},
	{"store/members.go", "Store.CanDecideApprovals", 1, readGated},
	{"store/members.go", "Store.ListChannelMembers", 1, readGated},
	{"store/members.go", "Store.ManagesChannel", 1, readGated},
	{"store/members.go", "Store.RemoveChannelMember", 1, readGated},
	{"store/members.go", "channelMemberTx", 1, readGated},
	{"store/mode.go", "Store.StartMode", 1, readOwnOrInternal},
	{"store/mode.go", "endPasswordOnlyTx", 1, readOwnOrInternal},
	{"store/operator_auth.go", "Store.CompleteTOTPLogin", 1, readOwnOrInternal},
	{"store/operator_auth.go", "Store.PendingLoginOf", 1, readOwnOrInternal},
	{"store/operator_auth.go", "Store.RedeemSetupToken", 1, readOwnOrInternal},
	{"store/operator_auth.go", "issueSetupTokenTx", 1, readOwnOrInternal},
	{"store/passwords.go", "Store.LoginCandidate", 1, readOwnOrInternal},
	// #1361: migration 44 asks only whether any principal exists.
	{"store/camera_default.go", "pinCameraOff", 1, readOwnOrInternal},
	{"store/passwords.go", "Store.PasswordSubject", 1, readOwnOrInternal},
	{"store/passwords.go", "bumpAuthGenerationTx", 1, readOwnOrInternal},
	{"store/passwords.go", "callerCredentialTx", 1, readOwnOrInternal},
	{"store/passwords.go", "recentAuthTx", 1, readOwnOrInternal},
	{"store/passwords.go", "recheckLoginTx", 1, readOwnOrInternal},
	{"store/permissions.go", "auditLinkSuspensionsTx", 1, readOwnOrInternal},
	{"store/permissions.go", "initializePermissions", 1, readOwnOrInternal},
	{"store/queries.go", "ownerProtectedTx", 1, readOwnOrInternal},
	{"store/queries.go", "queryPrincipal", 1, readOwnOrInternal},
	{"store/reactions.go", "reactionReaderTx", 1, readGated},
	{"store/rename.go", "Store.serverNameHoldersTx", 1, readOwnOrInternal},
	{"store/rename.go", "fillNameKeys", 1, readOwnOrInternal},
	{"store/reports.go", "Store.ProblemReportRedaction", 1, readOperatorOrMaker},
	{"store/resolve.go", "Store.CredentialLive", 1, readOwnOrInternal},
	{"store/resolve.go", "Store.resolveCredential", 1, readOwnOrInternal},
	{"store/restore.go", "Store.InvalidateRestored", 1, readOwnOrInternal},
	{"store/roles.go", "Store.BootstrapOperatorWith", 1, readOwnOrInternal},
	{"store/roles.go", "reportPromotedOwners", 1, readOperatorOrMaker},
	{"store/runs.go", "principalUsableInChannelTx", 1, readGated},
	{"store/sessions.go", "Store.CreateSession", 1, readOwnOrInternal},
	{"store/sessions.go", "Store.ListChannelSessions", 1, readGated},
	{"store/sessions.go", "Store.OnlinePrincipals", 1, readGated},
	{"store/sessions.go", "Store.OwnedSession", 1, readOwnOrInternal},
	{"store/sessions.go", "sessionByIDTx", 1, readOwnOrInternal},
	{"store/sign_in_links.go", "Store.CloseSignInLink", 1, readOwnOrInternal},
	{"store/sign_in_links.go", "Store.CreateConversionPermit", 1, readOwnOrInternal},
	{"store/sign_in_links.go", "Store.CreateDeviceLink", 1, readOwnOrInternal},
	{"store/sign_in_links.go", "Store.CreateSignInLink", 1, readOwnOrInternal},
	{"store/sign_in_links.go", "liveSignInLinkTx", 1, readOwnOrInternal},
	{"store/sign_in_links.go", "signInLinkCredentialLifetimeTx", 1, readOwnOrInternal},
	{"store/voice.go", "Store.InvalidVoiceRooms", 1, readOperatorOrMaker},
	{"store/voice_permission.go", "Store.SetVoiceAllowed", 2, readOperatorOrMaker},
}

// predicateNames are the identifiers a readPredicate function must use.
var predicateNames = map[string]string{
	"Store.ListDirectory":               "knownPrincipalsSQL",
	"mayKnowTx":                         "knownPrincipalsSQL",
	"directoryKnowersTx":                "knowsCond",
	"Store.DirectoryMembershipAudience": "knowsCondExcept",
}

var selectsPrincipals = regexp.MustCompile(`(?i)\b(from|join)\s+principals\b`)

func TestPrincipalReadsStayBehindTheMayKnowPredicate(t *testing.T) {
	type key struct{ file, fn string }
	found := map[key]int{}
	uses := map[key]map[string]bool{}
	authoritySources(t, func(_ *token.FileSet, rel string, file *ast.File) {
		if !strings.HasPrefix(rel, "store/") {
			return
		}
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			k := key{rel, funcName(fd)}
			ast.Inspect(fd, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BasicLit:
					if x.Kind != token.STRING {
						return true
					}
					if selectsPrincipals.MatchString(x.Value) {
						found[k]++
					}
					// The shared-channel clause is written once, in
					// store/directory.go.
					if rel != "store/directory.go" && strings.Contains(x.Value, "m1.principal_id") && strings.Contains(x.Value, "m2.principal_id") {
						t.Errorf("%s: %s repeats the shared-channel clause of the may-know predicate; use knownPrincipalsSQL", rel, k.fn)
					}
				case *ast.Ident:
					if uses[k] == nil {
						uses[k] = map[string]bool{}
					}
					uses[k][x.Name] = true
				}
				return true
			})
		}
	})
	if len(found) < 40 {
		t.Fatalf("only %d functions read principals; the scan is not finding the store", len(found))
	}
	listed := map[key]principalRead{}
	for _, e := range principalReads {
		k := key{e.file, e.fn}
		if _, dup := listed[k]; dup {
			t.Errorf("principalReads lists %s %s twice", e.file, e.fn)
		}
		listed[k] = e
	}
	for k, n := range found {
		e, ok := listed[k]
		switch {
		case !ok:
			t.Errorf("%s: %s reads the principals table (%d statement(s)) and is not in principalReads. If a member can be shown another principal by it, it goes through knownPrincipalsSQL; otherwise list it with the kind of read it is.", k.file, k.fn, n)
		case e.n != n:
			t.Errorf("%s: %s holds %d principals statement(s), principalReads says %d: look at the new one, then correct the entry", k.file, k.fn, n, e.n)
		}
	}
	var stale []string
	for k, e := range listed {
		if _, ok := found[k]; !ok {
			stale = append(stale, e.file+" "+e.fn)
		}
	}
	slices.Sort(stale)
	for _, s := range stale {
		t.Errorf("principalReads entry no longer matches the code: %s. Remove or correct it.", s)
	}
	for _, e := range principalReads {
		if e.kind != readPredicate {
			continue
		}
		want, ok := predicateNames[e.fn]
		if !ok {
			t.Errorf("%s is listed readPredicate but predicateNames does not say which fragment it must use", e.fn)
			continue
		}
		if !uses[key{e.file, e.fn}][want] {
			t.Errorf("%s is a reader of the may-know predicate and no longer uses %s", e.fn, want)
		}
	}
}

// TestPrincipalReadScanCatchesAnUnlistedQuery proves the scan sees a statement
// the way the guard needs: from, join, any case, any spacing, in a string.
func TestPrincipalReadScanCatchesAnUnlistedQuery(t *testing.T) {
	for _, sql := range []string{
		"SELECT name FROM principals WHERE id = ?",
		"select p.name from  principals p",
		"SELECT c.id FROM channel_members c JOIN principals p ON p.id = c.principal_id",
		"SELECT x FROM\n\tprincipals",
	} {
		if !selectsPrincipals.MatchString(sql) {
			t.Errorf("the scan misses %q", sql)
		}
	}
	for _, sql := range []string{
		"SELECT name FROM principals_archive",
		"UPDATE principals SET name = ?",
		"INSERT INTO principals (name) VALUES (?)",
		"SELECT id FROM channel_members",
	} {
		if selectsPrincipals.MatchString(sql) {
			t.Errorf("the scan flags %q, which does not read the table", sql)
		}
	}
}
