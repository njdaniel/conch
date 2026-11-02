package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The guests' rows (issue #717, design §9.5, §9.10, §9.14), run by the
// matrix's own runner (runRow) and held to the same three checks: the stated
// refusal, nothing of A's and now nothing private of the plaza's in any body
// or header, and the same answer as for something that never existed.
//
// guestRows run as guests of the plaza, one from the bare address and one
// through an invite, and the history rows again as the member one of them
// became (conversion.go): the plaza's history from the visit's bound and
// nothing before it, on every read route; nothing of A's; and every right a
// guest is refused. entryRows run as newcomers with no credential, each from
// an address of its own: the entry routes, which the matrix used to leave
// out because every request of the run came from one address and shared
// one allowance.

// The plaza's things from before the visits began, for pick.
func pPreRoot(w *world) subject { return subject{kind: "message", id: w.p.preRoot} }
func pPreFile(w *world) subject { return subject{kind: "file", id: w.p.preFile} }
func pPreRun(w *world) subject  { return subject{kind: "run", id: w.p.preRun} }
func pPreIV(w *world) subject   { return subject{kind: "intervention", id: w.p.preIV} }
func pPreWord(w *world) subject { return subject{name: w.p.preWord} }

// pch is the plaza's channel, for paths.
func pch(w *world) string { return url.PathEscape(w.p.channel) }

// historyRows are what a guest's bound means on every read route, and what
// the member a guest became keeps.
func historyRows() []row {
	return slices.Concat(coreHistoryRows(), reactionHistoryRows(), editHistoryRows())
}

func coreHistoryRows() []row {
	own := func(route, what string, inspect func(*world, api, []byte) error, p func(w *world) probe) row {
		return row{route: route, what: what, own: true, inspect: inspect, probe: func(w *world, _ subject) probe { return p(w) }}
	}
	threadNotFound := answer{http.StatusNotFound, "thread_not_found"}
	runNotFound := answer{http.StatusNotFound, "run_not_found"}
	return []row{
		own("GET /v0/channels/{channel}/messages", "reads the plaza (v0)", seesControl, func(w *world) probe { return get("/v0/channels/" + pch(w) + "/messages") }),
		own("GET /v1/channels/{channel}/messages", "reads the plaza (v1)", seesControl, func(w *world) probe { return get("/v1/channels/" + pch(w) + "/messages") }),
		own("GET /v2/channels/{channel}/messages", "reads the plaza (v2)", seesControl, func(w *world) probe { return get("/v2/channels/" + pch(w) + "/messages") }),
		own("GET /v2/channels/{channel}/messages", "reads the plaza from its first message on", seesControl, func(w *world) probe {
			return get("/v2/channels/" + pch(w) + "/messages?after=0&limit=100")
		}),
		own("GET /v2/channels/{channel}/messages", "reads the plaza newest first", seesControl, func(w *world) probe {
			return get("/v2/channels/" + pch(w) + "/messages?order=desc")
		}),
		{route: "GET /v2/channels/{channel}/messages", what: "reads the thread from before her visit", pick: pPreRoot, want: threadNotFound,
			probe: func(w *world, s subject) probe { return get("/v2/channels/" + pch(w) + "/messages?thread=" + id(s.id)) }},
		{route: "POST /v2/channels/{channel}/messages", what: "replies in the thread from before her visit", pick: pPreRoot, want: threadNotFound,
			probe: func(w *world, s subject) probe {
				return post("/v2/channels/"+pch(w)+"/messages", schema.PostMessageRequestV2{Body: "a reply", ThreadRootID: s.id})
			}},
		{route: "POST /v2/channels/{channel}/messages", what: "attaches the file from before her visit", pick: pPreFile, want: answer{http.StatusBadRequest, "invalid_file_attachment"},
			probe: func(w *world, s subject) probe {
				return post("/v2/channels/"+pch(w)+"/messages", attachment("see attached", s.id))
			}},
		{route: "GET /v2/channels/{channel}/messages/search", what: "searches the plaza for a word from before her visit", pick: pPreWord, own: true,
			probe: func(w *world, s subject) probe {
				return get("/v2/channels/" + pch(w) + "/messages/search?" + url.Values{"q": {s.name}}.Encode())
			}},
		// And search finds what she may see, so that the row above finding
		// nothing means something.
		own("GET /v2/channels/{channel}/messages/search", "searches the plaza for the message posted for her", seesControl, func(w *world) probe {
			return get("/v2/channels/" + pch(w) + "/messages/search?" + url.Values{"q": {w.p.control}}.Encode())
		}),
		{route: "GET /v2/files/{id}/meta", what: "reads the metadata of the file from before her visit", pick: pPreFile, want: answer{http.StatusNotFound, "file_not_found"},
			probe: func(_ *world, s subject) probe { return get("/v2/files/" + id(s.id) + "/meta") }},
		{route: "GET /v2/files/{id}", what: "downloads the file from before her visit", pick: pPreFile, want: answer{http.StatusNotFound, "file_not_found"},
			probe: func(_ *world, s subject) probe { return get("/v2/files/" + id(s.id)) }},
		own("GET /v2/files/{id}", "downloads the file shared since her visit began", nil, func(w *world) probe { return get("/v2/files/" + id(w.p.postFile)) }),
		{route: "GET /v1/runs/{id}", what: "reads the run from before her visit", pick: pPreRun, want: runNotFound,
			probe: func(_ *world, s subject) probe { return get("/v1/runs/" + id(s.id)) }},
		{route: "GET /v1/runs/{id}/activity", what: "reads the activity of the run from before her visit", pick: pPreRun, want: runNotFound,
			probe: func(_ *world, s subject) probe { return get("/v1/runs/" + id(s.id) + "/activity") }},
		{route: "GET /v1/runs/{id}/interventions", what: "reads the interventions of the run from before her visit", pick: pPreRun, want: runNotFound,
			probe: func(_ *world, s subject) probe { return get("/v1/runs/" + id(s.id) + "/interventions") }},
		{route: "GET /v1/interventions/{id}", what: "reads the intervention from before her visit", pick: pPreIV, want: answer{http.StatusNotFound, "intervention_not_found"},
			probe: func(_ *world, s subject) probe { return get("/v1/interventions/" + id(s.id)) }},
		own("GET /v1/runs/{id}", "reads the run on a message since her visit began", nil, func(w *world) probe { return get("/v1/runs/" + id(w.p.postRun)) }),
		own("GET /v1/runs", "lists the plaza's runs", nil, func(w *world) probe { return get("/v1/runs?channel=" + url.QueryEscape(w.p.channel)) }),
		own("GET /v1/sessions", "reads the plaza's roster", nil, func(w *world) probe { return get("/v1/sessions?channel=" + url.QueryEscape(w.p.channel)) }),
		own("GET /v1/channels/{channel}/members", "lists the plaza's members", nil, func(w *world) probe { return get("/v1/channels/" + pch(w) + "/members") }),
		own("GET /v1/channels/{channel}/nets", "lists the plaza's nets", nil, func(w *world) probe { return get("/v1/channels/" + pch(w) + "/nets") }),
		// An unread count is a number, which the marker scan cannot read: it
		// must be the messages after the mark she may see, so a count that
		// took in the plaza's messages from before her bound fails.
		own("GET /v1/read-state", "reads her read state", unreadInList, func(*world) probe { return get("/v1/read-state") }),
		// The mark is the smaller of the number given and the newest message
		// she may see: a message from before her bound is a number, never
		// looked up (as in the matrix's row of the same route). The count
		// after a mark there is everything she may see, and nothing before.
		{route: "PUT /v1/channels/{channel}/read-state", what: "marks the plaza read up to the message from before her visit", pick: pPreRoot, own: true,
			noGhost: "the mark is min(the number given, her newest message): the number is never looked up",
			inspect: unreadInMark,
			after:   func(w *world, a api) error { return readClampIn(w, a, w.p.channel) },
			probe: func(w *world, s subject) probe {
				return put("/v1/channels/"+pch(w)+"/read-state", schema.MarkReadRequestV1{LastReadID: s.id})
			}},

		// A's things: nothing of them, as for a member of B.
		{route: "GET /v2/channels/{channel}/messages", what: "reads A's channel", pick: aChannel, want: channelNotFound,
			probe: func(_ *world, s subject) probe { return get("/v2/channels/" + url.PathEscape(s.name) + "/messages") }},
		{route: "GET /v2/files/{id}", what: "downloads A's file", pick: aFile, want: answer{http.StatusNotFound, "file_not_found"},
			probe: func(_ *world, s subject) probe { return get("/v2/files/" + id(s.id)) }},
		{route: "GET /v1/runs/{id}", what: "reads A's run", pick: aRun, want: runNotFound,
			probe: func(_ *world, s subject) probe { return get("/v1/runs/" + id(s.id)) }},
		{route: "GET /v1/sessions", what: "reads A's channel's roster", pick: aChannel, want: channelNotFound,
			probe: func(_ *world, s subject) probe { return get("/v1/sessions?channel=" + url.QueryEscape(s.name)) }},
		{route: "POST /v1/channels/{channel}/voice/session", what: "asks for a voice session in A's channel", pick: aChannel, want: channelNotFound,
			probe: func(_ *world, s subject) probe { return post(ch(s)+"/voice/session", nil) }},
		{route: "GET /v1/voice/ws", what: "opens the voice presence socket of A's channel", pick: aChannel, want: channelNotFound,
			probe: func(_ *world, s subject) probe { return sock("/v1/voice/ws", s.name, nil, false) }},
		{route: "GET /v2/ws", what: "opens the v2 message socket of A's channel with a ticket", pick: aChannel, want: channelNotFound,
			probe: func(_ *world, s subject) probe { return sock("/v2/ws", s.name, nil, true) }},
		{route: "DELETE /v1/credentials/mine/{id}", what: "ends an A member's sign-in", pick: aCredential, want: answer{http.StatusNotFound, "credential_not_found"},
			probe: func(_ *world, s subject) probe { return del("/v1/credentials/mine/" + id(s.id)) }},
	}
}

// guestRows are the history rows and what a guest may not do, and what it
// is told about itself.
func guestRows() []row {
	refused := func(route, what string, want answer, p func(w *world) probe) row {
		return row{route: route, what: what, want: want, probe: func(w *world, _ subject) probe { return p(w) }}
	}
	notAllowed := answer{http.StatusForbidden, schema.ErrorCodeGuestNotAllowed}
	rows := []row{
		{route: "GET /v1/whoami", what: "asks who she is", own: true, inspect: isTheVisit,
			probe: func(*world, subject) probe { return get("/v1/whoami") }},
		{route: "GET /v1/channels", what: "lists her channels", own: true, inspect: onlyThePlaza,
			probe: func(*world, subject) probe { return get("/v1/channels") }},
		{route: "GET /v1/credentials/mine", what: "lists her sign-ins", own: true, inspect: oneSignIn,
			probe: func(*world, subject) probe { return get("/v1/credentials/mine") }},
		{route: "GET /v1/channels/{channel}/voice", what: "reads the plaza's voice presence (a guest holds no voice since A4, #1016)", want: answer{http.StatusForbidden, schema.ErrorCodeVoiceNotAllowed},
			probe: func(w *world, _ subject) probe { return get("/v1/channels/" + pch(w) + "/voice") }},
		{route: "POST /v1/reports", what: "reports a problem", own: true,
			probe: func(*world, subject) probe {
				return post("/v1/reports", schema.CreateProblemReportRequestV1{Did: "d", Expected: "e", Happened: "h", Often: schema.ProblemReportOftenOnce,
					Client: schema.ProblemReportClientV1{Name: "isolation-check", Version: "0.0.0", Platform: "linux"}, Diagnostics: []schema.DiagnosticEventV1{}})
			}},
		// A permit is judged alone (design §3.3): an A member's sign-in link
		// presented as one is the one conversion_permit_invalid, the same
		// bytes as for a code nobody was given, and nothing is converted.
		{route: "POST /v1/guest/convert", what: "creates an account with an A member's sign-in link as her permit", pick: aSignInCode,
			want: answer{http.StatusNotFound, schema.ErrorCodeConversionPermitInvalid},
			probe: func(_ *world, s subject) probe {
				permit := s.name
				if !strings.HasPrefix(permit, schema.SignInLinkCodePrefix) {
					permit = schema.SignInLinkCodePrefix + strings.Repeat("Q", schema.SignInLinkCodeLength-len(schema.SignInLinkCodePrefix))
				}
				return post("/v1/guest/convert", schema.ConvertGuestRequestV1{Name: "a-name-of-her-own", Password: proberPassword, Permit: permit})
			}},

		// What a guest may not do (design §4.3), whatever she names.
		refused("POST /v1/invites", "invites someone to the plaza", notAllowed, func(w *world) probe {
			return post("/v1/invites", schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{w.p.channel}})
		}),
		refused("POST /v2/channels/{channel}/files", "uploads a file to the plaza", notAllowed, func(w *world) probe {
			return probe{method: http.MethodPost, path: "/v2/channels/" + pch(w) + "/files?filename=x.txt", raw: []byte("x"), contentType: "text/plain"}
		}),
		refused("POST /v1/runs/{id}/interventions", "asks the plaza's agent to stop its run", notAllowed, func(w *world) probe {
			return post("/v1/runs/"+id(w.p.postRun)+"/interventions", schema.CreateInterventionRequestV1{Kind: schema.InterventionKindMessage, Body: "stop"})
		}),
		refused("POST /v2/channels/{channel}/messages", "whispers to a member", notAllowed, func(w *world) probe {
			return post("/v2/channels/"+pch(w)+"/messages", schema.PostMessageRequestV2{Body: "psst",
				Audience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{w.p.pia.id}}})
		}),
		// Approvals (issue #720, design §7.2): a guest can neither raise, see
		// nor decide one. The refusal is guest_not_allowed before anything is
		// looked up, so A's approval and one that never existed answer alike.
		refused("POST /v1/approvals", "raises an approval in the plaza", notAllowed, func(w *world) probe {
			return post("/v1/approvals", schema.CreateApprovalRequestV1{RequesterID: w.prober.id, ChannelID: w.p.chanID, Title: "t", Body: "b",
				Options:  []schema.Option{{ID: "yes", Label: "Yes", Kind: schema.OptionKindApprove}, {ID: "no", Label: "No", Kind: schema.OptionKindReject}},
				Deadline: schema.NewTimestamp(time.Now().Add(time.Hour))})
		}),
		refused("GET /v1/approvals", "lists the open approvals", notAllowed, func(*world) probe { return get("/v1/approvals") }),
		{route: "GET /v1/approvals/{id}", what: "reads A's approval", pick: aApproval, want: notAllowed,
			probe: func(_ *world, s subject) probe { return get("/v1/approvals/" + id(s.id)) }},
		{route: "POST /v1/approvals/{id}/decisions", what: "decides A's approval", pick: aApproval, want: notAllowed,
			probe: func(_ *world, s subject) probe {
				return post("/v1/approvals/"+id(s.id)+"/decisions", schema.CastDecisionRequestV1{OptionID: "yes", Reason: "r"})
			}},
		refused("POST /v1/sign-in-links/mine", "makes a device link", notAllowed, func(*world) probe {
			return post("/v1/sign-in-links/mine", schema.CreateDeviceLinkRequestV1{Label: "a phone"})
		}),
		refused("POST /v1/password/mine", "sets a password", notAllowed, func(*world) probe {
			return post("/v1/password/mine", schema.SetPasswordRequestV1{Password: proberPassword})
		}),
		refused("PUT /v1/password/mine", "changes a password", notAllowed, func(*world) probe {
			return put("/v1/password/mine", schema.ChangePasswordRequestV1{Current: guessedPassword, Password: proberPassword})
		}),
		refused("POST /v1/reauth", "proves a password", notAllowed, func(*world) probe {
			return post("/v1/reauth", schema.ReauthRequestV1{Password: guessedPassword})
		}),
		// Her name is her handle (review F10).
		refused("PUT /v1/principals/{id}/name", "renames herself", answer{http.StatusConflict, schema.ErrorCodeGuestNotHideable}, func(w *world) probe {
			return put("/v1/principals/"+id(w.prober.id)+"/name", schema.RenamePrincipalRequestV1{Name: "a-name-of-her-own"})
		}),
		// The operator's routes, about herself.
		refused("POST /v1/guests/{id}/conversion-permit", "makes herself a conversion permit", operatorOnly, func(w *world) probe {
			return post("/v1/guests/"+id(w.prober.id)+"/conversion-permit", schema.CreateConversionPermitRequestV1{})
		}),
		refused("POST /v1/principals/{id}/credentials", "issues herself a second credential", operatorOnly, func(w *world) probe {
			return post("/v1/principals/"+id(w.prober.id)+"/credentials", schema.CreateCredentialRequestV1{Label: "second"})
		}),
		refused("PUT /v1/principals/{id}/invite-permission", "lets herself invite", operatorOnly, func(w *world) probe {
			return put("/v1/principals/"+id(w.prober.id)+"/invite-permission", schema.InvitePermissionRequestV1{MayInvite: true})
		}),
		refused("PUT /v1/principals/{id}/voice-permission", "switches her own voice on", operatorOnly, func(w *world) probe {
			return put("/v1/principals/"+id(w.prober.id)+"/voice-permission", schema.VoicePermissionRequestV1{VoiceAllowed: true})
		}),
		refused("PUT /v1/join/settings", "opens the bare address wider", operatorOnly, func(*world) probe {
			return put("/v1/join/settings", schema.PutJoinSettingsRequestV1{MaxGuests: ptr(int64(schema.MaxMaxGuests))})
		}),
		refused("GET /v1/guests", "lists the visits", operatorOnly, func(*world) probe { return get("/v1/guests") }),
		refused("POST /v1/guests/{id}/end", "ends another guest's visit", operatorOnly, func(w *world) probe {
			return post("/v1/guests/"+id(w.p.nick2.id)+"/end", nil)
		}),
		refused("POST /v0/channels", "makes a channel", operatorOnly, func(*world) probe {
			return post("/v0/channels", schema.CreateChannelRequest{Name: "a-channel-of-her-own"})
		}),
	}
	return append(historyRows(), rows...)
}

// entryRows are the entry routes as a newcomer tries them, each from an
// address of its own, while the plaza is open to accounts. Guests are
// removed for now (issue #1005): the guest entry routes refuse every try
// with guests_disabled before the label or the code is read.
func entryRows() []row {
	badLabel := func(what, label string) row {
		return row{route: "POST /v1/join/guest", what: "joins as a guest under " + what, want: guestsDisabled,
			probe: func(*world, subject) probe { return post("/v1/join/guest", schema.JoinGuestRequestV1{Label: label}) }}
	}
	takenName := func(what string, name func(w *world) string) row {
		return row{route: "POST /v1/join/account", what: "creates an account under " + what, want: answer{http.StatusConflict, schema.ErrorCodePrincipalExists},
			probe: func(w *world, _ subject) probe {
				return post("/v1/join/account", schema.JoinAccountRequestV1{Name: name(w), Password: joinPassword})
			}}
	}
	// The password routes answer only a request that came over HTTPS
	// (design §5.4): through the trusted proxy, its X-Forwarded-Proto must
	// say https. Here the proxy says http, or says nothing of the protocol.
	// (Plain HTTP from a peer that is not loopback and not a trusted proxy
	// is the unit tests': every peer of this run is loopback. See
	// internal/server/clientaddr_test.go TestClientAddr and login_test.go
	// TestSecureTransport.)
	httpsRequired := answer{http.StatusForbidden, schema.ErrorCodeHTTPSRequired}
	overHTTP := map[string]string{"X-Forwarded-Proto": "http"}
	noProto := map[string]string{"X-Forwarded-Proto": ""}
	unknownInvite := schema.InviteCodePrefix + strings.Repeat("Q", schema.InviteCodeLength-len(schema.InviteCodePrefix))
	unknownLink := schema.SignInLinkCodePrefix + strings.Repeat("Q", schema.SignInLinkCodeLength-len(schema.SignInLinkCodePrefix))
	return []row{
		{route: "GET /v1/join", what: "reads the front door", own: true, inspect: offersAccounts,
			probe: func(*world, subject) probe { return get("/v1/join") }},
		{route: "POST /v1/join/guest", what: "joins as a guest at the bare address", want: guestsDisabled,
			probe: func(*world, subject) probe { return post("/v1/join/guest", schema.JoinGuestRequestV1{Label: "Kim"}) }},
		{route: "POST /v1/invites/redeem-guest", what: "joins as a guest through the plaza's invite for guests", want: guestsDisabled,
			probe: func(w *world, _ subject) probe {
				return post("/v1/invites/redeem-guest", schema.RedeemInviteGuestRequestV1{Code: w.p.invites[schema.InviteJoinGuest].code, Label: "Kim"})
			}},
		badLabel("a label with a bidi control", "Ki\u202em"),
		badLabel("a label with a control character", "Ki\u0007m"),
		badLabel("a label that fakes a second tag", "Kim \u00b7 a7k4"),
		badLabel("a label in the reserved guest namespace", "Guest-kim"),
		badLabel("a label that is the server's name", "conch"),
		{route: "POST /v1/join/account", what: "creates an account in the reserved guest namespace", want: answer{http.StatusBadRequest, "invalid_request"},
			probe: func(*world, subject) probe {
				return post("/v1/join/account", schema.JoinAccountRequestV1{Name: "Guest-kim", Password: joinPassword})
			}},
		// U+0455 is the Cyrillic letter that looks like s: the name's
		// skeleton begins with guest, which no permanent name may.
		takenName("a name that only looks like a guest's", func(*world) string { return "gue\u0455t-kim" }),
		takenName("the server's name", func(*world) string { return "conch" }),
		takenName("a member's name", func(w *world) string { return w.p.pia.name }),
		{route: "POST /v1/login", what: "signs in through a proxy that says she came over plain HTTP", want: httpsRequired,
			probe: func(w *world, _ subject) probe {
				p := post("/v1/login", schema.LoginRequestV1{Name: w.p.pia.name, Password: joinPassword})
				p.headers = overHTTP
				return p
			}},
		{route: "POST /v1/login", what: "signs in through a proxy that does not say how she came", want: httpsRequired,
			probe: func(w *world, _ subject) probe {
				p := post("/v1/login", schema.LoginRequestV1{Name: w.p.pia.name, Password: joinPassword})
				p.headers = noProto
				return p
			}},
		{route: "POST /v1/join/account", what: "creates an account through a proxy that says she came over plain HTTP", want: httpsRequired,
			probe: func(*world, subject) probe {
				p := post("/v1/join/account", schema.JoinAccountRequestV1{Name: "a-name-of-her-own", Password: joinPassword})
				p.headers = overHTTP
				return p
			}},
		{route: "POST /v1/join/account", what: "creates an account through a proxy that does not say how she came", want: httpsRequired,
			probe: func(*world, subject) probe {
				p := post("/v1/join/account", schema.JoinAccountRequestV1{Name: "a-name-of-her-own", Password: joinPassword})
				p.headers = noProto
				return p
			}},
		{route: "POST /v1/invites/redeem-guest", what: "joins as a guest with a code nobody gave her", want: guestsDisabled,
			probe: func(*world, subject) probe {
				return post("/v1/invites/redeem-guest", schema.RedeemInviteGuestRequestV1{Code: unknownInvite, Label: "Kim"})
			}},
		{route: "POST /v2/invites/redeem", what: "redeems a code nobody gave her", want: answer{http.StatusNotFound, schema.ErrorCodeInviteInvalid},
			probe: func(*world, subject) probe {
				return post("/v2/invites/redeem", schema.RedeemInviteRequestV2{Code: unknownInvite, Name: "a-name-of-her-own", Password: joinPassword})
			}},
		// v1 makes no account with any code (#713, design §7.6 B1).
		{route: "POST /v1/invites/redeem", what: "redeems the plaza's invite without a password (v1)", want: answer{http.StatusConflict, schema.ErrorCodePasswordRequired},
			probe: func(w *world, _ subject) probe {
				return post("/v1/invites/redeem", schema.RedeemInviteRequestV1{Code: w.p.invites[schema.InviteJoinBoth].code, Name: "a-name-of-her-own"})
			}},
		{route: "POST /v1/invites/preview", what: "previews a code nobody gave her", want: answer{http.StatusNotFound, schema.ErrorCodeInviteInvalid},
			probe: func(*world, subject) probe {
				return post("/v1/invites/preview", schema.InvitePreviewRequestV1{Code: unknownInvite})
			}},
		{route: "POST /v1/sign-in-links/preview", what: "previews a sign-in link nobody gave her", want: answer{http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
			probe: func(*world, subject) probe {
				return post("/v1/sign-in-links/preview", schema.SignInLinkPreviewRequestV1{Code: unknownLink})
			}},
		{route: "POST /v1/sign-in-links/redeem", what: "signs in with a sign-in link nobody gave her", want: answer{http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid},
			probe: func(*world, subject) probe {
				return post("/v1/sign-in-links/redeem", schema.RedeemSignInLinkRequestV1{Code: unknownLink})
			}},
	}
}

// runEntryRows runs every entry row as a newcomer of its own.
func (w *world) runEntryRows() (int, error) {
	var failures []string
	n := 0
	for _, r := range entryRows() {
		w.prober = &member{name: "a newcomer", api: w.newcomer("a newcomer")}
		k, err := w.runRow(r)
		n += k
		if err != nil {
			failures = append(failures, fmt.Sprintf("  %s (a newcomer %s): %v", r.route, r.what, err))
		}
	}
	if len(failures) > 0 {
		return n, fmt.Errorf("%d of the entry rows failed:\n%s", len(failures), strings.Join(failures, "\n"))
	}
	return n, nil
}

// seesControl requires that a history the prober was given holds the
// message every guest was meant to see, so that what it does not hold
// means something.
func seesControl(w *world, _ api, body []byte) error {
	if !bytes.Contains(body, []byte(w.p.control)) {
		return fmt.Errorf("the history does not hold the message posted for her since her visit began")
	}
	return nil
}

// isTheVisit requires that whoami says a guest, with the visit she joined
// with, and no right to invite.
func isTheVisit(w *world, _ api, body []byte) error {
	var who schema.WhoAmIResponseV1
	if err := json.Unmarshal(body, &who); err != nil {
		return err
	}
	g := w.prober.guest
	if who.IdentityClass != schema.IdentityGuest || who.Guest == nil || who.Guest.Handle != g.Handle || who.Guest.Tag != g.Tag ||
		who.Guest.Label != g.Label || who.Name != g.Handle || who.MayInvite || who.Role != schema.RoleMember {
		return fmt.Errorf("whoami says %s %q (%s), visit %+v, may invite %v; want the guest %q", who.Role, who.Name, who.IdentityClass, who.Guest, who.MayInvite, g.Handle)
	}
	return nil
}

// onlyThePlaza requires that a guest's channels are the plaza and nothing
// else.
func onlyThePlaza(w *world, _ api, body []byte) error {
	var list schema.ListChannelsResponse
	if err := json.Unmarshal(body, &list); err != nil {
		return err
	}
	if len(list.Channels) != 1 || list.Channels[0].ID != w.p.chanID {
		return fmt.Errorf("a guest's channels are %v, want the plaza alone", list.Channels)
	}
	return nil
}

// oneSignIn requires that a guest has one sign-in: the visit's.
func oneSignIn(w *world, _ api, body []byte) error {
	var mine schema.ListOwnCredentialsResponseV1
	if err := json.Unmarshal(body, &mine); err != nil {
		return err
	}
	if len(mine.Credentials) != 1 || mine.Credentials[0].ID != w.prober.credID || !mine.Credentials[0].Current {
		return fmt.Errorf("a guest's sign-ins are %+v, want the visit's credential %d alone", mine.Credentials, w.prober.credID)
	}
	return nil
}

// offersAccounts requires that the front door offers an account and no
// visit: guests are removed for now (issue #1005).
func offersAccounts(_ *world, _ api, body []byte) error {
	var info schema.JoinInfoV1
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&info); err != nil {
		return err
	}
	if info.Join.Guest || info.Join.GuestsFull || !info.Join.Account || info.ServerName != "conch" || !info.PasswordLogin {
		return fmt.Errorf("the front door says %+v, want an account and no visit at conch", info)
	}
	return nil
}

// visibleAfter is how many messages of channel after id a sees in its v2
// history, excluding a's own posts: what an unread count after a mark at id must be.
func visibleAfter(a api, channel string, after int64) (int, error) {
	var who schema.WhoAmIResponseV1
	if err := a.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return 0, err
	}
	n := 0
	for {
		var page schema.ListMessagesResponseV2
		if err := a.call(http.MethodGet, fmt.Sprintf("/v2/channels/%s/messages?after=%d&limit=100", url.PathEscape(channel), after), nil, &page); err != nil {
			return 0, err
		}
		for _, message := range page.Messages {
			if message.AuthorID != who.ID {
				n++
			}
		}
		if page.NextAfter <= after {
			return n, nil
		}
		after = page.NextAfter
	}
}

// unreadMatches requires that a read state's count is what a sees after
// its mark.
func unreadMatches(w *world, a api, rs schema.ReadStateV1) error {
	want, err := visibleAfter(a, w.p.channel, rs.LastReadID)
	if err != nil {
		return err
	}
	if rs.Unread != want {
		return fmt.Errorf("the plaza's unread count after %d is %d; %s sees %d messages after it", rs.LastReadID, rs.Unread, a.who, want)
	}
	return nil
}

// unreadInList is the read-state list's check, for the plaza's row.
func unreadInList(w *world, a api, body []byte) error {
	var list schema.ListReadStateResponseV1
	if err := json.Unmarshal(body, &list); err != nil {
		return err
	}
	for _, rs := range list.ReadState {
		if rs.ChannelID == w.p.chanID {
			return unreadMatches(w, a, rs)
		}
	}
	return fmt.Errorf("her read state has no row for the plaza")
}

// unreadInMark is the mark's check.
func unreadInMark(w *world, a api, body []byte) error {
	var rs schema.ReadStateV1
	if err := json.Unmarshal(body, &rs); err != nil {
		return err
	}
	if rs.LastReadID != w.p.preRoot || rs.Unread == 0 {
		return fmt.Errorf("a mark at the message from before her visit is stored as %d with %d unread, want it kept and the messages since counted", rs.LastReadID, rs.Unread)
	}
	return unreadMatches(w, a, rs)
}

// readClampIn is the read-state rows' own check in channel: a mark past
// every message is stored as the newest message the prober can see there.
func readClampIn(w *world, a api, channel string) error {
	var mark schema.ReadStateV1
	if err := a.call(http.MethodPut, "/v1/channels/"+url.PathEscape(channel)+"/read-state", schema.MarkReadRequestV1{LastReadID: w.ghost().id}, &mark); err != nil {
		return err
	}
	if leaks := w.leaks(mustJSON(mark), asked{}); len(leaks) > 0 {
		return fmt.Errorf("the read mark holds %s", strings.Join(leaks, "; "))
	}
	var newest int64
	for after := int64(0); ; {
		var page schema.ListMessagesResponseV2
		if err := a.call(http.MethodGet, fmt.Sprintf("/v2/channels/%s/messages?after=%d&limit=100", url.PathEscape(channel), after), nil, &page); err != nil {
			return err
		}
		for _, m := range page.Messages {
			newest = max(newest, m.ID)
		}
		if page.NextAfter <= after {
			break
		}
		after = page.NextAfter
	}
	if mark.LastReadID != newest {
		return fmt.Errorf("a mark at %d is stored as %d, want %d, the newest message %s can see in %s", w.ghost().id, mark.LastReadID, newest, a.who, channel)
	}
	return nil
}
