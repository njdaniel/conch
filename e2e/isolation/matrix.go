package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/pkg/schema"
)

// The matrix: for every route of the table, what a member of group B tries
// against what group A made, and what the answer must be.
//
// The matrix runs twice, as two members of B (w.prober): bea, who joined
// through the operator's invite, may invite, holds voice and is in both of
// B's channels, so she holds every right a tester can; and ola, whom the
// operator made by hand, in B's first channel only. The rows an agent's
// credential reaches differently run a third time as B's agent (agentRows).
// Each row names one of A's objects (by name or id) or, for a route that
// only ever answers about the caller, nothing of A at all.
//
// Every answer is checked three ways:
//
//   - A refusal has the status and code the row states, and its body is
//     exactly an error document (code and message, nothing else).
//   - No body or response header, refusal or not, holds a marker of A
//     (markers.go) beyond the name or object the request itself named, and
//     an error message holds no number but that id.
//   - The same request naming something that never existed (a channel nobody
//     made, an id far past any in use) gets the same answer, byte for byte
//     once the name or id is set aside. So a refusal cannot tell B that A's
//     object exists: B learns nothing it could not learn by guessing.
//
// A row is data: adding a route to routes.go means adding a line here, and
// the coverage check (routes.go in this directory) fails the run until
// someone does.

// answer is a refusal: a status and an error code.
type answer struct {
	status int
	code   string
}

var (
	channelNotFound = answer{http.StatusNotFound, "channel_not_found"}
	operatorOnly    = answer{http.StatusForbidden, "forbidden"}
)

// subject is the thing of A's a request names (or, for the ghost request,
// something that never existed).
type subject struct {
	kind string // the kind of id (markers.go): "channel", "run", ...
	name string
	id   int64
}

// probe is one request.
type probe struct {
	method, path string
	body         any    // a JSON body, or nil
	raw          []byte // a raw body, sent with contentType, instead of body
	contentType  string
	socket       bool // a WebSocket upgrade (method GET)
	ticket       bool // with a ticket rather than the bearer
}

// row is one line of the matrix.
type row struct {
	route string // the routes.go pattern this request reaches
	what  string // what B tries, for messages
	// pick is the A object the request names; nil when it names none.
	pick  func(w *world) subject
	probe func(w *world, s subject) probe
	// want is the refusal; own rows have none.
	want answer
	// own is true for a route that answers only about the caller: it must
	// succeed, and hold nothing of A.
	own bool
	// spare sends the request with B's spare credential (it signs out).
	spare bool
	// noGhost says why a row that names A's object has no ghost
	// comparison: the route never looks the thing up.
	noGhost string
	// onBehalf rows name an A member as the one acting (author_id,
	// requester_id, principal_id) on B's own channel or approval: the answer
	// is a refusal, or a success bound to the caller, never one as A.
	onBehalf bool
	// after runs once the row has passed, as the prober, for a check of its
	// own.
	after func(w *world, a api) error
	// mayNot is the refusal for a prober an operator did not let invite,
	// when it differs from want.
	mayNot answer
}

// wantFor is the refusal the row states for prober.
func (r row) wantFor(prober *member) answer {
	if !prober.mayInvite && r.mayNot != (answer{}) {
		return r.mayNot
	}
	return r.want
}

// A's objects, for pick.
func aChannel(w *world) subject {
	return subject{kind: "channel", name: w.a.channels[0], id: w.a.chanIDs[0]}
}
func aMember(w *world) subject       { return subject{kind: "principal", id: w.a.people[0].id} }
func aAgent(w *world) subject        { return subject{kind: "principal", id: w.a.agent.id} }
func aCredential(w *world) subject   { return subject{kind: "credential", id: w.a.people[0].credID} }
func aMessage(w *world) subject      { return subject{kind: "message", id: w.a.root} }
func aFile(w *world) subject         { return subject{kind: "file", id: w.a.fileID} }
func aNet(w *world) subject          { return subject{kind: "net", name: w.a.netName, id: w.a.netID} }
func aSession(w *world) subject      { return subject{kind: "session", id: w.a.sessionID} }
func aRun(w *world) subject          { return subject{kind: "run", id: w.a.runID} }
func aIntervention(w *world) subject { return subject{kind: "intervention", id: w.a.interID} }
func aApproval(w *world) subject     { return subject{kind: "approval", id: w.a.approval} }
func aOpenLink(w *world) subject     { return subject{kind: "invite", id: w.a.openInviteID} }
func aPersonal(w *world) subject     { return subject{kind: "invite", id: w.a.invites[1]} }
func aSignInLink(w *world) subject   { return subject{kind: "sign_in_link", id: w.a.signInLink} }
func aReport(w *world) subject       { return subject{kind: "report", id: w.a.reportID} }
func aMemberName(w *world) subject   { return subject{kind: "principal", name: w.a.people[0].name} }
func aWord(w *world) subject         { return subject{name: w.aName("word-root")} }

// ghost is the subject of the comparison request: nothing by that name or id
// was ever made.
func (w *world) ghost() subject { return subject{name: "ghost-" + w.btag, id: 987654321} }

// request builders.
func get(path string) probe            { return probe{method: http.MethodGet, path: path} }
func del(path string) probe            { return probe{method: http.MethodDelete, path: path} }
func post(path string, body any) probe { return probe{method: http.MethodPost, path: path, body: body} }
func put(path string, body any) probe  { return probe{method: http.MethodPut, path: path, body: body} }
func sock(path, channel string, q url.Values, ticket bool) probe {
	if q == nil {
		q = url.Values{}
	}
	q.Set("channel", channel)
	return probe{method: http.MethodGet, path: path + "?" + q.Encode(), socket: true, ticket: ticket}
}

func ch(s subject) string { return "/v1/channels/" + url.PathEscape(s.name) }
func id(n int64) string   { return itoa(n) }

// matrixRows is the matrix.
func matrixRows() []row {
	byChannel := func(route, what string, p func(w *world, s subject) probe) row {
		return row{route: route, what: what, pick: aChannel, probe: p, want: channelNotFound}
	}
	operator := func(route, what string, pick func(*world) subject, p func(w *world, s subject) probe) row {
		return row{route: route, what: what, pick: pick, probe: p, want: operatorOnly}
	}
	own := func(route, what string, p func(w *world, s subject) probe) row {
		return row{route: route, what: what, probe: p, own: true}
	}
	bChannel := func(w *world) string { return w.b.main() }
	transmit := schema.VoiceTransmitReportV1{State: schema.VoiceTransmitStateStarted}
	return []row{
		// Sockets: refused before the upgrade.
		byChannel("GET /v0/ws", "opens the v0 message socket of A's channel", func(_ *world, s subject) probe { return sock("/v0/ws", s.name, nil, false) }),
		byChannel("GET /v1/ws", "opens the v1 message socket of A's channel", func(_ *world, s subject) probe { return sock("/v1/ws", s.name, nil, false) }),
		byChannel("GET /v2/ws", "opens the v2 message socket of A's channel", func(_ *world, s subject) probe { return sock("/v2/ws", s.name, nil, false) }),
		byChannel("GET /v2/ws", "opens the v2 message socket of A's channel with a ticket", func(_ *world, s subject) probe { return sock("/v2/ws", s.name, nil, true) }),
		byChannel("GET /v1/events/ws", "opens the events socket of A's channel", func(_ *world, s subject) probe { return sock("/v1/events/ws", s.name, nil, false) }),
		byChannel("GET /v1/events/ws", "opens the events socket of A's channel with a ticket", func(_ *world, s subject) probe { return sock("/v1/events/ws", s.name, nil, true) }),
		{route: "GET /v1/events/ws", what: "opens the events socket of B's channel keeping A's session alive", pick: aSession,
			want: answer{http.StatusNotFound, "session_not_found"},
			probe: func(w *world, s subject) probe {
				return sock("/v1/events/ws", bChannel(w), url.Values{"session": {id(s.id)}}, true)
			}},
		byChannel("GET /v1/voice/ws", "opens the voice presence socket of A's channel", func(_ *world, s subject) probe { return sock("/v1/voice/ws", s.name, nil, false) }),
		byChannel("GET /v1/voice/ws", "opens the voice presence socket of A's channel with a ticket", func(_ *world, s subject) probe { return sock("/v1/voice/ws", s.name, nil, true) }),
		own("POST /v1/ws-tickets", "asks for a socket ticket", func(*world, subject) probe { return post("/v1/ws-tickets", nil) }),

		// Who B is, and B's channels.
		own("GET /v1/whoami", "asks who she is", func(*world, subject) probe { return get("/v1/whoami") }),
		own("GET /v1/channels", "lists her channels", func(*world, subject) probe { return get("/v1/channels") }),
		operator("POST /v0/channels", "makes a channel with A's channel's name", aChannel, func(_ *world, s subject) probe {
			return post("/v0/channels", schema.CreateChannelRequest{Name: s.name})
		}),
		operator("POST /v0/principals", "makes a principal with an A member's name", aMemberName, func(_ *world, s subject) probe {
			return post("/v0/principals", schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: s.name})
		}),
		operator("PUT /v1/principals/{id}/name", "renames an A member", aMember, func(_ *world, s subject) probe {
			return put("/v1/principals/"+id(s.id)+"/name", schema.RenamePrincipalRequestV1{Name: "renamed"})
		}),

		// Messages, every version, search and threads.
		byChannel("POST /v0/channels/{channel}/messages", "posts to A's channel (v0)", func(w *world, s subject) probe {
			return post("/v0/channels/"+s.name+"/messages", schema.PostMessageRequest{AuthorID: w.prober.id, Body: "hello"})
		}),
		byChannel("GET /v0/channels/{channel}/messages", "reads A's channel (v0)", func(_ *world, s subject) probe { return get("/v0/channels/" + s.name + "/messages") }),
		byChannel("POST /v1/channels/{channel}/messages", "posts to A's channel (v1)", func(w *world, s subject) probe {
			return post(ch(s)+"/messages", schema.PostMessageRequestV1{AuthorID: w.prober.id, Body: "hello"})
		}),
		byChannel("GET /v1/channels/{channel}/messages", "reads A's channel (v1)", func(_ *world, s subject) probe { return get(ch(s) + "/messages") }),
		byChannel("POST /v2/channels/{channel}/messages", "posts to A's channel (v2)", func(_ *world, s subject) probe {
			return post("/v2/channels/"+s.name+"/messages", schema.PostMessageRequestV2{Body: "hello"})
		}),
		byChannel("GET /v2/channels/{channel}/messages", "reads A's channel (v2)", func(_ *world, s subject) probe { return get("/v2/channels/" + s.name + "/messages") }),
		{route: "GET /v2/channels/{channel}/messages", what: "reads A's thread in A's channel", pick: aChannel, want: channelNotFound,
			probe: func(w *world, s subject) probe {
				return get("/v2/channels/" + s.name + "/messages?thread=" + id(w.a.root))
			}},
		byChannel("GET /v2/channels/{channel}/messages/search", "searches A's channel", func(w *world, s subject) probe {
			return get("/v2/channels/" + s.name + "/messages/search?" + url.Values{"q": {aWord(w).name}}.Encode())
		}),
		{route: "GET /v2/channels/{channel}/messages", what: "reads A's thread through her own channel", pick: aMessage,
			want: answer{http.StatusNotFound, "thread_not_found"},
			probe: func(w *world, s subject) probe {
				return get("/v2/channels/" + bChannel(w) + "/messages?thread=" + id(s.id))
			}},
		{route: "POST /v2/channels/{channel}/messages", what: "replies in her own channel to A's message", pick: aMessage,
			want: answer{http.StatusNotFound, "thread_not_found"},
			probe: func(w *world, s subject) probe {
				return post("/v2/channels/"+bChannel(w)+"/messages", schema.PostMessageRequestV2{Body: "a reply", ThreadRootID: s.id})
			}},
		{route: "POST /v2/channels/{channel}/messages", what: "whispers in her own channel to an A member", pick: aMember,
			want: answer{http.StatusBadRequest, "invalid_audience"},
			probe: func(w *world, s subject) probe {
				return post("/v2/channels/"+bChannel(w)+"/messages", schema.PostMessageRequestV2{Body: "psst",
					Audience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{s.id}}})
			}},
		{route: "POST /v2/channels/{channel}/messages", what: "posts in her own channel to A's net", pick: aNet,
			want: answer{http.StatusNotFound, "net_not_found"},
			probe: func(w *world, s subject) probe {
				return post("/v2/channels/"+bChannel(w)+"/messages", schema.PostMessageRequestV2{Body: "to the net",
					Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: s.id}})
			}},
		{route: "POST /v2/channels/{channel}/messages", what: "attaches A's file to a message in her own channel", pick: aFile,
			want: answer{http.StatusBadRequest, "invalid_file_attachment"},
			probe: func(w *world, s subject) probe {
				data, _ := json.Marshal(schema.FileAttachmentV1{FileIDs: []int64{s.id}})
				return post("/v2/channels/"+bChannel(w)+"/messages", schema.PostMessageRequestV2{Body: "see attached",
					Payload: &schema.Payload{Schema: schema.FileAttachmentV1Name, Data: data}})
			}},

		// B's own views hold nothing of A either.
		own("GET /v0/channels/{channel}/messages", "reads her own channel (v0)", func(w *world, _ subject) probe { return get("/v0/channels/" + bChannel(w) + "/messages") }),
		own("GET /v1/channels/{channel}/messages", "reads her own channel (v1)", func(w *world, _ subject) probe { return get("/v1/channels/" + bChannel(w) + "/messages") }),
		own("GET /v2/channels/{channel}/messages", "reads her own channel (v2)", func(w *world, _ subject) probe { return get("/v2/channels/" + bChannel(w) + "/messages") }),
		{route: "GET /v2/channels/{channel}/messages/search", what: "searches her own channel for A's words", pick: aWord, own: true,
			probe: func(w *world, s subject) probe {
				return get("/v2/channels/" + bChannel(w) + "/messages/search?" + url.Values{"q": {s.name}}.Encode())
			}},
		own("GET /v1/channels/{channel}/members", "lists her own channel's members", func(w *world, _ subject) probe { return get("/v1/channels/" + bChannel(w) + "/members") }),
		own("GET /v1/channels/{channel}/nets", "lists her own channel's nets", func(w *world, _ subject) probe { return get("/v1/channels/" + bChannel(w) + "/nets") }),
		own("GET /v1/sessions", "reads her own channel's roster", func(w *world, _ subject) probe { return get("/v1/sessions?channel=" + bChannel(w)) }),
		own("GET /v1/runs", "lists her own channel's runs", func(w *world, _ subject) probe { return get("/v1/runs?channel=" + bChannel(w)) }),
		own("GET /v1/channels/{channel}/voice", "reads her own channel's voice presence", func(w *world, _ subject) probe { return get("/v1/channels/" + bChannel(w) + "/voice") }),

		// Files.
		{route: "POST /v2/channels/{channel}/files", what: "uploads a file to A's channel", pick: aChannel, want: channelNotFound,
			probe: func(_ *world, s subject) probe {
				return probe{method: http.MethodPost, path: "/v2/channels/" + s.name + "/files?filename=x.txt", raw: []byte("x"), contentType: "text/plain"}
			}},
		{route: "GET /v2/files/{id}/meta", what: "reads the metadata of A's file", pick: aFile, want: answer{http.StatusNotFound, "file_not_found"},
			probe: func(_ *world, s subject) probe { return get("/v2/files/" + id(s.id) + "/meta") }},
		{route: "GET /v2/files/{id}", what: "downloads A's file", pick: aFile, want: answer{http.StatusNotFound, "file_not_found"},
			probe: func(_ *world, s subject) probe { return get("/v2/files/" + id(s.id)) }},

		// Members and nets.
		byChannel("GET /v1/channels/{channel}/members", "lists A's channel's members", func(_ *world, s subject) probe { return get(ch(s) + "/members") }),
		operator("PUT /v1/channels/{channel}/members/{principal_id}", "adds herself to A's channel", aChannel, func(w *world, s subject) probe {
			return put(ch(s)+"/members/"+id(w.prober.id), nil)
		}),
		operator("DELETE /v1/channels/{channel}/members/{principal_id}", "removes an A member from A's channel", aChannel, func(w *world, s subject) probe {
			return del(ch(s) + "/members/" + id(w.a.people[0].id))
		}),
		operator("POST /v1/channels/{channel}/nets", "makes a net in A's channel", aChannel, func(_ *world, s subject) probe {
			return post(ch(s)+"/nets", schema.CreateNetRequestV1{Name: "b-net"})
		}),
		byChannel("GET /v1/channels/{channel}/nets", "lists A's channel's nets", func(_ *world, s subject) probe { return get(ch(s) + "/nets") }),
		operator("DELETE /v1/channels/{channel}/nets/{net}", "archives A's net", aNet, func(w *world, s subject) probe {
			return del("/v1/channels/" + w.a.channels[0] + "/nets/" + s.name)
		}),
		operator("PUT /v1/channels/{channel}/nets/{net}/members/{principal_id}", "joins A's net", aNet, func(w *world, s subject) probe {
			return put("/v1/channels/"+w.a.channels[0]+"/nets/"+s.name+"/members/"+id(w.prober.id), schema.PutNetMemberRequestV1{Role: schema.NetRoleMember})
		}),
		operator("DELETE /v1/channels/{channel}/nets/{net}/members/{principal_id}", "removes an A member from A's net", aNet, func(w *world, s subject) probe {
			return del("/v1/channels/" + w.a.channels[0] + "/nets/" + s.name + "/members/" + id(w.a.people[0].id))
		}),

		// Voice and the share.
		byChannel("POST /v1/channels/{channel}/voice/session", "asks for a voice session in A's channel", func(_ *world, s subject) probe { return post(ch(s)+"/voice/session", nil) }),
		byChannel("POST /v1/channels/{channel}/voice/transmit", "reports transmitting in A's channel", func(_ *world, s subject) probe { return post(ch(s)+"/voice/transmit", transmit) }),
		byChannel("POST /v1/channels/{channel}/voice/share", "claims A's room's share", func(_ *world, s subject) probe { return post(ch(s)+"/voice/share", nil) }),
		byChannel("DELETE /v1/channels/{channel}/voice/share", "stops A's room's share", func(_ *world, s subject) probe { return del(ch(s) + "/voice/share") }),
		byChannel("GET /v1/channels/{channel}/voice", "reads A's voice presence", func(_ *world, s subject) probe { return get(ch(s) + "/voice") }),

		// Sessions, runs and interventions.
		own("POST /v1/sessions", "opens a session of her own", func(*world, subject) probe {
			return post("/v1/sessions", schema.CreateSessionRequestV1{Client: "bravo-matrix"})
		}),
		{route: "POST /v1/sessions/{id}/heartbeat", what: "keeps A's agent's session alive", pick: aSession, want: answer{http.StatusNotFound, "session_not_found"},
			probe: func(_ *world, s subject) probe { return post("/v1/sessions/"+id(s.id)+"/heartbeat", nil) }},
		{route: "DELETE /v1/sessions/{id}", what: "ends A's agent's session", pick: aSession, want: answer{http.StatusNotFound, "session_not_found"},
			probe: func(_ *world, s subject) probe { return del("/v1/sessions/" + id(s.id)) }},
		byChannel("GET /v1/sessions", "reads A's channel's roster", func(_ *world, s subject) probe { return get("/v1/sessions?channel=" + url.QueryEscape(s.name)) }),
		{route: "POST /v1/sessions/{id}/runs", what: "starts a run on A's agent's session", pick: aSession, want: answer{http.StatusForbidden, schema.ErrorCodeAgentRequired},
			probe: func(w *world, s subject) probe {
				return post("/v1/sessions/"+id(s.id)+"/runs", schema.CreateRunRequestV1{Channel: bChannel(w), Task: "t"})
			}},
		{route: "POST /v1/runs/{id}/state", what: "sets the state of A's run", pick: aRun, want: answer{http.StatusNotFound, "run_not_found"},
			probe: func(_ *world, s subject) probe {
				return post("/v1/runs/"+id(s.id)+"/state", schema.SetRunStateRequestV1{State: schema.RunStateCompleted})
			}},
		{route: "POST /v1/runs/{id}/activity", what: "adds activity to A's run", pick: aRun, want: answer{http.StatusNotFound, "run_not_found"},
			probe: func(_ *world, s subject) probe {
				return post("/v1/runs/"+id(s.id)+"/activity", schema.AppendActivityRequestV1{Kind: schema.ActivityKindRunStarted, Summary: "s"})
			}},
		{route: "GET /v1/runs/{id}", what: "reads A's run", pick: aRun, want: answer{http.StatusNotFound, "run_not_found"},
			probe: func(_ *world, s subject) probe { return get("/v1/runs/" + id(s.id)) }},
		byChannel("GET /v1/runs", "lists A's channel's runs", func(_ *world, s subject) probe { return get("/v1/runs?channel=" + url.QueryEscape(s.name)) }),
		{route: "GET /v1/runs", what: "lists her own channel's runs of A's agent's session", pick: aSession, own: true,
			probe: func(w *world, s subject) probe {
				return get("/v1/runs?channel=" + bChannel(w) + "&session=" + id(s.id))
			}},
		{route: "GET /v1/runs/{id}/activity", what: "reads A's run's activity", pick: aRun, want: answer{http.StatusNotFound, "run_not_found"},
			probe: func(_ *world, s subject) probe { return get("/v1/runs/" + id(s.id) + "/activity") }},
		{route: "POST /v1/runs/{id}/interventions", what: "asks A's run to stop", pick: aRun, want: answer{http.StatusNotFound, "run_not_found"},
			probe: func(_ *world, s subject) probe {
				return post("/v1/runs/"+id(s.id)+"/interventions", schema.CreateInterventionRequestV1{Kind: schema.InterventionKindMessage, Body: "stop"})
			}},
		{route: "GET /v1/runs/{id}/interventions", what: "reads A's run's interventions", pick: aRun, want: answer{http.StatusNotFound, "run_not_found"},
			probe: func(_ *world, s subject) probe { return get("/v1/runs/" + id(s.id) + "/interventions") }},
		{route: "GET /v1/interventions/{id}", what: "reads A's intervention", pick: aIntervention, want: answer{http.StatusNotFound, "intervention_not_found"},
			probe: func(_ *world, s subject) probe { return get("/v1/interventions/" + id(s.id)) }},
		{route: "GET /v1/sessions/{id}/interventions", what: "polls A's agent's session for interventions", pick: aSession, want: answer{http.StatusNotFound, "session_not_found"},
			probe: func(_ *world, s subject) probe { return get("/v1/sessions/" + id(s.id) + "/interventions") }},
		{route: "POST /v1/interventions/{id}/ack", what: "answers A's intervention", pick: aIntervention, want: answer{http.StatusNotFound, "intervention_not_found"},
			probe: func(_ *world, s subject) probe {
				return post("/v1/interventions/"+id(s.id)+"/ack", schema.AckInterventionRequestV1{State: schema.InterventionStateAcknowledged})
			}},

		// Read state.
		own("GET /v1/read-state", "reads her read state", func(*world, subject) probe { return get("/v1/read-state") }),
		byChannel("PUT /v1/channels/{channel}/read-state", "marks A's channel read", func(w *world, s subject) probe {
			return put(ch(s)+"/read-state", schema.MarkReadRequestV1{LastReadID: 1})
		}),
		// The mark is stored as the smaller of the number given and the newest
		// message B can see in her channel: a number, never looked up, so the
		// answer is the same whether or not A has a message by that id, and
		// a ghost id (larger than any) is no comparison. checkReadClamp
		// checks that rule instead.
		{route: "PUT /v1/channels/{channel}/read-state", what: "marks her own channel read up to A's message", pick: aMessage, own: true,
			noGhost: "the mark is min(the number given, B's newest message): the number is never looked up",
			after:   checkReadClamp,
			probe: func(w *world, s subject) probe {
				return put("/v1/channels/"+bChannel(w)+"/read-state", schema.MarkReadRequestV1{LastReadID: s.id})
			}},

		// Acting as an A member on B's own channel and approval: the server
		// binds the actor to the caller or refuses, and never acts as A.
		{route: "POST /v0/channels/{channel}/messages", what: "posts in her own channel as an A member (v0)", pick: aMember, onBehalf: true,
			probe: func(w *world, s subject) probe {
				return post("/v0/channels/"+bChannel(w)+"/messages", schema.PostMessageRequest{AuthorID: s.id, Body: "as someone else"})
			}},
		{route: "POST /v1/channels/{channel}/messages", what: "posts in her own channel as an A member (v1)", pick: aMember, onBehalf: true,
			probe: func(w *world, s subject) probe {
				return post("/v1/channels/"+bChannel(w)+"/messages", schema.PostMessageRequestV1{AuthorID: s.id, Body: "as someone else"})
			}},
		{route: "POST /v2/channels/{channel}/messages", what: "posts in her own channel as an A member (v2)", pick: aMember, onBehalf: true,
			probe: func(w *world, s subject) probe {
				return post("/v2/channels/"+bChannel(w)+"/messages", schema.PostMessageRequestV2{AuthorID: s.id, Body: "as someone else"})
			}},
		{route: "POST /v1/approvals", what: "raises an approval in her own channel as an A member", pick: aMember, onBehalf: true,
			probe: func(w *world, s subject) probe {
				return post("/v1/approvals", schema.CreateApprovalRequestV1{RequesterID: s.id, ChannelID: w.b.chanIDs[0], Title: "t", Body: "b",
					Options:  []schema.Option{{ID: "yes", Label: "Yes", Kind: schema.OptionKindApprove}, {ID: "no", Label: "No", Kind: schema.OptionKindReject}},
					Deadline: schema.NewTimestamp(time.Now().Add(time.Hour))})
			}},
		{route: "POST /v1/approvals/{id}/decisions", what: "decides B's approval as an A member", pick: aMember, onBehalf: true,
			probe: func(w *world, s subject) probe {
				return post("/v1/approvals/"+id(w.b.approval)+"/decisions", schema.CastDecisionRequestV1{PrincipalID: s.id, OptionID: "no", Reason: "r"})
			}},

		// Principals, manifests and credentials: operators (or the principal).
		operator("PUT /v1/principals/{id}/manifest", "writes A's agent's manifest", aAgent, func(_ *world, s subject) probe {
			return put("/v1/principals/"+id(s.id)+"/manifest", schema.PutAgentManifestRequestV1{DisplayName: "x", Tier: schema.AgentTierA})
		}),
		operator("GET /v1/principals/{id}/manifest", "reads A's agent's manifest", aAgent, func(_ *world, s subject) probe {
			return get("/v1/principals/" + id(s.id) + "/manifest")
		}),
		operator("POST /v1/principals/{id}/credentials", "issues a credential for an A member", aMember, func(_ *world, s subject) probe {
			return post("/v1/principals/"+id(s.id)+"/credentials", schema.CreateCredentialRequestV1{Label: "x"})
		}),
		operator("GET /v1/principals/{id}/credentials", "lists an A member's credentials", aMember, func(_ *world, s subject) probe {
			return get("/v1/principals/" + id(s.id) + "/credentials")
		}),
		operator("POST /v1/credentials/{credential_id}/rotate", "rotates an A member's credential", aCredential, func(_ *world, s subject) probe {
			return post("/v1/credentials/"+id(s.id)+"/rotate", nil)
		}),
		operator("DELETE /v1/credentials/{credential_id}", "revokes an A member's credential", aCredential, func(_ *world, s subject) probe {
			return del("/v1/credentials/" + id(s.id))
		}),
		{route: "DELETE /v1/credentials/current", what: "signs out (her spare credential)", spare: true, own: true,
			probe: func(*world, subject) probe { return del("/v1/credentials/current") }},
		operator("POST /v1/principals/{id}/disable", "disables an A member", aMember, func(_ *world, s subject) probe {
			return post("/v1/principals/"+id(s.id)+"/disable", nil)
		}),
		operator("POST /v1/principals/{id}/enable", "enables an A member", aMember, func(_ *world, s subject) probe {
			return post("/v1/principals/"+id(s.id)+"/enable", nil)
		}),
		operator("POST /v1/principals/{id}/hide", "hides an A member", aMember, func(_ *world, s subject) probe {
			return post("/v1/principals/"+id(s.id)+"/hide", nil)
		}),
		operator("POST /v1/principals/hide", "hides everyone from A's open link", aOpenLink, func(_ *world, s subject) probe {
			return post("/v1/principals/hide", schema.HidePrincipalsRequestV1{InviteID: s.id})
		}),
		operator("GET /v1/principals", "lists the principals", nil, func(*world, subject) probe { return get("/v1/principals") }),
		{route: "POST /v1/principals/{id}/credentials/revoke-all", what: "signs an A member out everywhere", pick: aMember,
			want: answer{http.StatusForbidden, "forbidden"},
			probe: func(_ *world, s subject) probe {
				return post("/v1/principals/"+id(s.id)+"/credentials/revoke-all", nil)
			}},
		operator("GET /v1/audit", "reads the audit log", nil, func(*world, subject) probe { return get("/v1/audit") }),
		operator("POST /v1/hooks", "makes a hook into A's channel", aChannel, func(w *world, s subject) probe {
			return post("/v1/hooks", schema.CreateHookRequest{Channel: s.name, Principal: w.prober.id})
		}),
		operator("GET /v1/hooks", "lists the hooks", nil, func(*world, subject) probe { return get("/v1/hooks") }),
		operator("DELETE /v1/hooks/{id}", "revokes a hook", nil, func(*world, subject) probe { return del("/v1/hooks/1") }),

		// Invites.
		{route: "POST /v1/invites", what: "invites someone to A's channel", pick: aChannel,
			want: answer{http.StatusForbidden, schema.ErrorCodeInviteGrantExceedsInviter}, mayNot: answer{http.StatusForbidden, schema.ErrorCodeInviteNotAllowed},
			probe: func(_ *world, s subject) probe {
				return post("/v1/invites", schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{s.name}})
			}},
		own("GET /v1/invites", "lists her invites", func(*world, subject) probe { return get("/v1/invites") }),
		{route: "DELETE /v1/invites/{id}", what: "closes A's open link", pick: aOpenLink, want: answer{http.StatusNotFound, schema.ErrorCodeInviteNotFound},
			probe: func(_ *world, s subject) probe { return del("/v1/invites/" + id(s.id)) }},
		{route: "DELETE /v1/invites/{id}", what: "closes an A tester's personal invite", pick: aPersonal, want: answer{http.StatusNotFound, schema.ErrorCodeInviteNotFound},
			probe: func(_ *world, s subject) probe { return del("/v1/invites/" + id(s.id)) }},
		operator("GET /v1/invites/{id}/principals", "lists who joined through A's open link", aOpenLink, func(_ *world, s subject) probe {
			return get("/v1/invites/" + id(s.id) + "/principals")
		}),
		operator("POST /v1/invites/{id}/disable-joiners", "disables everyone from A's open link", aOpenLink, func(_ *world, s subject) probe {
			return post("/v1/invites/"+id(s.id)+"/disable-joiners", schema.DisableJoinersRequestV1{DryRun: true})
		}),
		operator("POST /v1/principals/{id}/disable-invitees", "disables an A tester's invitees", aMember, func(_ *world, s subject) probe {
			return post("/v1/principals/"+id(s.id)+"/disable-invitees", schema.DisableInviteesRequestV1{DryRun: true})
		}),
		operator("PUT /v1/principals/{id}/invite-permission", "takes an A tester's right to invite", aMember, func(_ *world, s subject) probe {
			return put("/v1/principals/"+id(s.id)+"/invite-permission", schema.InvitePermissionRequestV1{})
		}),
		operator("GET /v1/invites/settings", "reads the invite settings", nil, func(*world, subject) probe { return get("/v1/invites/settings") }),
		operator("PUT /v1/invites/settings", "pauses invites", nil, func(*world, subject) probe {
			return put("/v1/invites/settings", schema.InviteSettingsV1{Paused: true})
		}),

		// Sign-in links (issue #493): an operator's.
		operator("POST /v1/principals/{id}/sign-in-links", "makes a sign-in link for an A member", aMember, func(_ *world, s subject) probe {
			return post("/v1/principals/"+id(s.id)+"/sign-in-links", schema.CreateSignInLinkRequestV1{})
		}),
		operator("DELETE /v1/sign-in-links/{id}", "closes an A member's sign-in link", aSignInLink, func(_ *world, s subject) probe {
			return del("/v1/sign-in-links/" + id(s.id))
		}),

		// Problem reports.
		own("POST /v1/reports", "reports a problem", func(w *world, _ subject) probe {
			return post("/v1/reports", schema.CreateProblemReportRequestV1{Did: "d", Expected: "e", Happened: "h", Often: schema.ProblemReportOftenOnce,
				Client: schema.ProblemReportClientV1{Name: "isolation-check", Version: "0.0.0", Platform: "linux"}, Diagnostics: []schema.DiagnosticEventV1{}})
		}),
		operator("GET /v1/reports", "lists the reports", nil, func(*world, subject) probe { return get("/v1/reports") }),
		operator("GET /v1/reports/{id}", "reads A's report", aReport, func(_ *world, s subject) probe { return get("/v1/reports/" + id(s.id)) }),
		operator("PUT /v1/reports/{id}", "files A's report", aReport, func(_ *world, s subject) probe {
			return put("/v1/reports/"+id(s.id), schema.UpdateProblemReportRequestV1{State: schema.ProblemReportStateFiled, Issue: "https://example.invalid/1"})
		}),

		// Approvals: a B member can neither see nor decide A's.
		{route: "POST /v1/approvals", what: "raises an approval in A's channel", pick: aChannel, want: answer{http.StatusBadRequest, "channel_not_found"},
			probe: func(w *world, s subject) probe {
				return post("/v1/approvals", schema.CreateApprovalRequestV1{RequesterID: w.prober.id, ChannelID: s.id, Title: "t", Body: "b",
					Options:  []schema.Option{{ID: "yes", Label: "Yes", Kind: schema.OptionKindApprove}, {ID: "no", Label: "No", Kind: schema.OptionKindReject}},
					Deadline: schema.NewTimestamp(time.Now().Add(time.Hour))})
			}},
		own("GET /v1/approvals", "lists the open approvals", func(*world, subject) probe { return get("/v1/approvals") }),
		{route: "GET /v1/approvals/{id}", what: "reads A's approval", pick: aApproval, want: answer{http.StatusNotFound, "approval_not_found"},
			probe: func(_ *world, s subject) probe { return get("/v1/approvals/" + id(s.id)) }},
		{route: "POST /v1/approvals/{id}/decisions", what: "decides A's approval", pick: aApproval, want: answer{http.StatusNotFound, "approval_not_found"},
			probe: func(w *world, s subject) probe {
				return post("/v1/approvals/"+id(s.id)+"/decisions", schema.CastDecisionRequestV1{PrincipalID: w.prober.id, OptionID: "yes", Reason: "r"})
			}},
	}
}

// exempt are the routes with no row, and why. Each is a route no group owns
// anything behind.
var exempt = map[string]string{
	"GET /healthz":                   "takes no credential and answers the same to everyone: the server's health and version",
	"/mcp":                           "the agents' surface, with its own bearer authentication and the manifest and membership gates; no B member is an agent (the MCP tests cover it)",
	"POST /v1/hooks/{token}":         "authenticated by the hook's own URL token, which only an operator is shown",
	"POST /v1/invites/preview":       "takes an invite code and answers only with what that code grants; B does not hold A's codes, and every other code answers the one invite_invalid (the revocation step checks a closed one)",
	"POST /v1/invites/redeem":        "as preview: the code is the credential; a closed or unknown one answers invite_invalid (the revocation step checks it)",
	"POST /v1/sign-in-links/preview": "takes a sign-in link's code and answers only with the name it signs in as; only an operator is shown a code, and A's are never given to B",
	"POST /v1/sign-in-links/redeem":  "as preview: the code is the credential, made by an operator for one principal; B holds no code of A's",
}

// result is what one request was answered.
type result struct {
	status int
	body   []byte
	header http.Header
}

// send makes the request of p as a.
func (w *world) send(a api, p probe) (result, error) {
	if p.socket {
		path, rawQuery, _ := strings.Cut(p.path, "?")
		q, err := url.ParseQuery(rawQuery)
		if err != nil {
			return result{}, err
		}
		f, r, err := dialSocket(a, "a socket", path, q, p.ticket)
		if err != nil {
			return result{}, err
		}
		if f != nil {
			f.close()
			return result{status: http.StatusSwitchingProtocols}, nil
		}
		return result(r), nil
	}
	raw, contentType := p.raw, p.contentType
	if raw == nil && p.body != nil {
		encoded, err := json.Marshal(p.body)
		if err != nil {
			return result{}, err
		}
		raw, contentType = encoded, "application/json"
	}
	status, body, header, err := a.request(p.method, p.path, contentType, raw)
	return result{status: status, body: body, header: header}, err
}

// runMatrix runs every row as prober and returns how many requests it made.
// Every row runs; the failures are reported together.
func (w *world) runMatrix(prober *member, rows []row) (int, error) {
	w.prober = prober
	var failures []string
	n := 0
	for _, r := range rows {
		k, err := w.runRow(r)
		n += k
		if err != nil {
			failures = append(failures, fmt.Sprintf("  %s (%s %s): %v", r.route, prober.name, r.what, err))
		}
	}
	if len(failures) > 0 {
		return n, fmt.Errorf("%d of the matrix's rows failed:\n%s", len(failures), strings.Join(failures, "\n"))
	}
	return n, nil
}

// runRow runs one row and its ghost.
func (w *world) runRow(r row) (int, error) {
	as := w.prober.api
	if r.spare {
		spare, ok := w.spares[w.prober.id]
		if !ok {
			return 0, nil // a credential the row spends; this prober has none to spare
		}
		as = spare
	}
	var s subject
	var a asked
	if r.pick != nil {
		s = r.pick(w)
		if s.id != 0 {
			a.ids = []kindID{{s.kind, s.id}}
		}
		if s.name != "" {
			a.strs = []string{s.name}
		}
	}
	p := r.probe(w, s)
	if err := w.checkRoute(r.route, p); err != nil {
		return 0, err
	}
	got, err := w.send(as, p)
	if err != nil {
		return 1, err
	}
	if leaks := w.marks.leaks(got.body, a); len(leaks) > 0 {
		return 1, fmt.Errorf("status %d, and the body holds %s", got.status, strings.Join(leaks, "; "))
	}
	if leaks := w.marks.headerLeaks(got.header, a); len(leaks) > 0 {
		return 1, fmt.Errorf("status %d, and the response headers hold %s", got.status, strings.Join(leaks, "; "))
	}
	success := got.status >= 200 && got.status <= 299
	switch {
	case r.onBehalf:
		if err := w.checkOnBehalf(got); err != nil {
			return 1, err
		}
		w.onBehalf = append(w.onBehalf, fmt.Sprintf("%s %d", r.route, got.status))
	case r.own:
		if !success {
			return 1, fmt.Errorf("status %d %s, want a success: the route answers about the caller alone", got.status, w.h.redact(lkit.Truncate(string(got.body), 200)))
		}
	default:
		if err := checkRefusal(got, r.wantFor(w.prober), a); err != nil {
			return 1, err
		}
	}
	if r.after != nil {
		if err := r.after(w, as); err != nil {
			return 1, err
		}
	}
	if r.pick == nil || r.noGhost != "" || (r.onBehalf && success) {
		return 1, nil
	}
	// The same request about something that never existed.
	g := w.ghost()
	g.kind = s.kind
	ghost, err := w.send(as, r.probe(w, g))
	if err != nil {
		return 2, err
	}
	if ghost.status != got.status || !bytes.Equal(normalize(ghost.body, g), normalize(got.body, s)) {
		return 2, fmt.Errorf("the answer about A's object is %d %s, and about one that never existed %d %s: the difference tells B that A's exists",
			got.status, strings.TrimSpace(string(got.body)), ghost.status, strings.TrimSpace(string(ghost.body)))
	}
	return 2, nil
}

// checkRefusal requires the status and code of want, a body that is exactly
// an error document, and a message with no number but the one asked about.
func checkRefusal(got result, want answer, a asked) error {
	if got.status != want.status {
		return fmt.Errorf("status %d %s, want %d %s", got.status, strings.TrimSpace(string(got.body)), want.status, want.code)
	}
	if got.body == nil && want.status == http.StatusSwitchingProtocols {
		return nil
	}
	var e schema.Error
	dec := json.NewDecoder(bytes.NewReader(got.body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return fmt.Errorf("status %d with a body that is not exactly an error document (%w): %s", got.status, err, strings.TrimSpace(string(got.body)))
	}
	if e.Code != want.code {
		return fmt.Errorf("status %d code %q (%q), want code %q", got.status, e.Code, e.Message, want.code)
	}
	if extra := numbersBeyond(e.Message, a); len(extra) > 0 {
		return fmt.Errorf("the error message %q holds numbers %v beyond the one asked about", e.Message, extra)
	}
	return nil
}

// normalize sets aside the name and id a request named, so that the answers
// about A's object and the ghost can be compared.
func normalize(body []byte, s subject) []byte {
	out := body
	if s.name != "" {
		out = bytes.ReplaceAll(out, []byte(s.name), []byte("<asked>"))
	}
	if s.id != 0 {
		out = bytes.ReplaceAll(out, []byte(itoa(s.id)), []byte("<asked>"))
	}
	return out
}

// checkReadClamp is the read-state row's own check: a mark past every message
// is stored as the newest message B can see in the channel.
func checkReadClamp(w *world, a api) error {
	channel := w.b.main()
	var mark schema.ReadStateV1
	if err := a.call(http.MethodPut, "/v1/channels/"+channel+"/read-state", schema.MarkReadRequestV1{LastReadID: w.ghost().id}, &mark); err != nil {
		return err
	}
	if leaks := w.marks.leaks(mustJSON(mark), asked{}); len(leaks) > 0 {
		return fmt.Errorf("the read mark holds %s", strings.Join(leaks, "; "))
	}
	var newest int64
	for after := int64(0); ; {
		var page schema.ListMessagesResponseV2
		if err := a.call(http.MethodGet, fmt.Sprintf("/v2/channels/%s/messages?after=%d&limit=100", channel, after), nil, &page); err != nil {
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
		return fmt.Errorf("a mark at %d is stored as %d, want %d, the newest message B can see in %s", w.ghost().id, mark.LastReadID, newest, channel)
	}
	return nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// checkOnBehalf requires that a request naming an A member as the actor was
// refused with an error document, or succeeded with every actor in the
// answer being the caller.
func (w *world) checkOnBehalf(got result) error {
	if got.status < 200 || got.status > 299 {
		if got.status < 400 || got.status > 499 {
			return fmt.Errorf("status %d %s, want a refusal or a success bound to the caller", got.status, strings.TrimSpace(string(got.body)))
		}
		var e schema.Error
		dec := json.NewDecoder(bytes.NewReader(got.body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			return fmt.Errorf("status %d with a body that is not exactly an error document (%w)", got.status, err)
		}
		return nil
	}
	var doc any
	if err := json.Unmarshal(got.body, &doc); err != nil {
		return err
	}
	actors := map[string]bool{"author_id": true, "requester_id": true, "principal_id": true}
	var wrong []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				if n, ok := c.(float64); ok && actors[k] && int64(n) != w.prober.id {
					wrong = append(wrong, fmt.Sprintf("%s=%d", k, int64(n)))
				}
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(doc)
	if len(wrong) > 0 {
		return fmt.Errorf("the request succeeded acting as someone else: %s (the caller is %d)", strings.Join(wrong, ", "), w.prober.id)
	}
	return nil
}
