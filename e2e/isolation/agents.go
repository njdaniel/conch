package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/pkg/schema"
)

// An agent of B's. A person is refused on the agent routes before anything is
// looked up, so the person's rows prove little about them. B's agent holds
// every capability and a grant on B's first channel, its own session there,
// and so reaches the lookups: these rows run as it, over REST (agentRows,
// through the matrix's own runner) and over MCP (mcpProbes).

// agentRows are the rows an agent reaches differently from a person.
func agentRows() []row { return slices.Concat(coreAgentRows(), reactionAgentRows(), editAgentRows()) }

func coreAgentRows() []row {
	byChannel := func(route, what string, p func(w *world, s subject) probe) row {
		return row{route: route, what: what, pick: aChannel, probe: p, want: channelNotFound}
	}
	byID := func(route, what string, pick func(*world) subject, want answer, p func(w *world, s subject) probe) row {
		return row{route: route, what: what, pick: pick, probe: p, want: want}
	}
	sessionNotFound := answer{http.StatusNotFound, "session_not_found"}
	// Agents keep no voice (ADR-004): occupancy is refused them, as presence
	// is, and the refusal is audited.
	noVoice := func(route, what string, p probe) row {
		return row{route: route, what: what, want: answer{http.StatusForbidden, "forbidden"}, after: checkAgentVoiceDenied(route),
			probe: func(*world, subject) probe { return p }}
	}
	runNotFound := answer{http.StatusNotFound, "run_not_found"}
	interNotFound := answer{http.StatusNotFound, "intervention_not_found"}
	return []row{
		// Channels are made by community members (issue #981): an agent is
		// refused before any name is looked at.
		byID("POST /v1/channels", "makes a channel with A's private channel's name", aChannel, answer{http.StatusForbidden, "forbidden"}, func(_ *world, s subject) probe {
			return post("/v1/channels", schema.CreateChannelRequestV1{Name: s.name, Visibility: schema.ChannelPrivate})
		}),
		byChannel("GET /v2/channels/{channel}/messages", "reads A's channel", func(_ *world, s subject) probe { return get("/v2/channels/" + s.name + "/messages") }),
		byChannel("POST /v2/channels/{channel}/messages", "posts to A's channel", func(_ *world, s subject) probe {
			return post("/v2/channels/"+s.name+"/messages", schema.PostMessageRequestV2{Body: "hello"})
		}),
		byChannel("GET /v1/sessions", "reads A's channel's roster", func(_ *world, s subject) probe { return get("/v1/sessions?channel=" + url.QueryEscape(s.name)) }),
		byChannel("GET /v1/runs", "lists A's channel's runs", func(_ *world, s subject) probe { return get("/v1/runs?channel=" + url.QueryEscape(s.name)) }),
		byChannel("GET /v1/events/ws", "opens the events socket of A's channel", func(_ *world, s subject) probe { return sock("/v1/events/ws", s.name, nil, true) }),
		byChannel("POST /v1/sessions/{id}/runs", "starts a run on its own session in A's channel", func(w *world, s subject) probe {
			return post("/v1/sessions/"+id(w.b.agentSession)+"/runs", schema.CreateRunRequestV1{Channel: s.name, Task: "t"})
		}),
		{route: "GET /v1/events/ws", what: "opens its own channel's events socket keeping A's session alive", pick: aSession, want: sessionNotFound,
			probe: func(w *world, s subject) probe {
				return sock("/v1/events/ws", w.b.main(), url.Values{"session": {id(s.id)}}, true)
			}},
		byID("POST /v1/sessions/{id}/heartbeat", "keeps A's agent's session alive", aSession, sessionNotFound, func(_ *world, s subject) probe {
			return post("/v1/sessions/"+id(s.id)+"/heartbeat", nil)
		}),
		byID("DELETE /v1/sessions/{id}", "ends A's agent's session", aSession, sessionNotFound, func(_ *world, s subject) probe { return del("/v1/sessions/" + id(s.id)) }),
		byID("POST /v1/sessions/{id}/runs", "starts a run on A's agent's session", aSession, sessionNotFound, func(w *world, s subject) probe {
			return post("/v1/sessions/"+id(s.id)+"/runs", schema.CreateRunRequestV1{Channel: w.b.main(), Task: "t"})
		}),
		byID("POST /v1/runs/{id}/state", "sets the state of A's run", aRun, runNotFound, func(_ *world, s subject) probe {
			return post("/v1/runs/"+id(s.id)+"/state", schema.SetRunStateRequestV1{State: schema.RunStateCompleted})
		}),
		byID("POST /v1/runs/{id}/activity", "adds activity to A's run", aRun, runNotFound, func(_ *world, s subject) probe {
			return post("/v1/runs/"+id(s.id)+"/activity", schema.AppendActivityRequestV1{Kind: schema.ActivityKindRunStarted, Summary: "s"})
		}),
		byID("GET /v1/runs/{id}", "reads A's run", aRun, runNotFound, func(_ *world, s subject) probe { return get("/v1/runs/" + id(s.id)) }),
		byID("GET /v1/runs/{id}/activity", "reads A's run's activity", aRun, runNotFound, func(_ *world, s subject) probe { return get("/v1/runs/" + id(s.id) + "/activity") }),
		byID("GET /v1/runs/{id}/interventions", "reads A's run's interventions", aRun, runNotFound, func(_ *world, s subject) probe {
			return get("/v1/runs/" + id(s.id) + "/interventions")
		}),
		byID("GET /v1/interventions/{id}", "reads A's intervention", aIntervention, interNotFound, func(_ *world, s subject) probe {
			return get("/v1/interventions/" + id(s.id))
		}),
		byID("GET /v1/sessions/{id}/interventions", "polls A's agent's session for interventions", aSession, sessionNotFound, func(_ *world, s subject) probe {
			return get("/v1/sessions/" + id(s.id) + "/interventions")
		}),
		byID("POST /v1/interventions/{id}/ack", "answers A's intervention", aIntervention, interNotFound, func(_ *world, s subject) probe {
			return post("/v1/interventions/"+id(s.id)+"/ack", schema.AckInterventionRequestV1{State: schema.InterventionStateAcknowledged})
		}),
		noVoice("GET /v1/voice/occupancy", "reads voice occupancy", get("/v1/voice/occupancy")),
		noVoice("GET /v1/voice/occupancy/ws", "opens the voice occupancy socket", occupancySock(false)),
		noVoice("GET /v1/voice/occupancy/ws", "opens the voice occupancy socket with a ticket", occupancySock(true)),
	}
}

// mcpProbe is one tool call of B's agent naming one of A's objects.
type mcpProbe struct {
	tool string
	pick func(w *world) subject
	args func(w *world, s subject) map[string]any
}

// mcpProbes are the agent tools that take a channel or an id: each is asked
// about A's object and about one that never existed. The approval tools are
// asked only what reads (check_decision, await_decision); nothing of the
// approval path is changed or exercised beyond a read.
func mcpProbes() []mcpProbe {
	return []mcpProbe{
		{"read_channel", aChannel, func(_ *world, s subject) map[string]any { return map[string]any{"channel": s.name} }},
		{"post_message", aChannel, func(_ *world, s subject) map[string]any { return map[string]any{"channel": s.name, "body": "hello"} }},
		{"check_decision", aApproval, func(_ *world, s subject) map[string]any { return map[string]any{"approval_id": s.id} }},
		{"await_decision", aApproval, func(_ *world, s subject) map[string]any { return map[string]any{"approval_id": s.id, "timeout_ms": 1} }},
		{"heartbeat", aSession, func(_ *world, s subject) map[string]any { return map[string]any{"session_id": s.id} }},
		{"end_session", aSession, func(_ *world, s subject) map[string]any { return map[string]any{"session_id": s.id} }},
		{"start_run", aSession, func(w *world, s subject) map[string]any {
			return map[string]any{"session_id": s.id, "channel": w.b.main(), "task": "t"}
		}},
		{"start_run", aChannel, func(w *world, s subject) map[string]any {
			return map[string]any{"session_id": w.b.agentSession, "channel": s.name, "task": "t"}
		}},
		{"update_run", aRun, func(_ *world, s subject) map[string]any {
			return map[string]any{"run_id": s.id, "state": string(schema.RunStateCompleted)}
		}},
		{"post_activity", aRun, func(_ *world, s subject) map[string]any {
			return map[string]any{"run_id": s.id, "kind": string(schema.ActivityKindRunStarted), "summary": "s"}
		}},
		{"poll_interventions", aSession, func(_ *world, s subject) map[string]any { return map[string]any{"session_id": s.id} }},
		{"ack_intervention", aIntervention, func(_ *world, s subject) map[string]any {
			return map[string]any{"intervention_id": s.id, "state": string(schema.InterventionStateAcknowledged)}
		}},
	}
}

// toolAnswer is what one tool call came back with: a refusal's code and
// message, or a result.
type toolAnswer struct {
	refused       bool
	code, message string
	text          string // everything the call returned, for the scan
}

func callTool(ctx context.Context, c *mcpclient.Client, tool string, args map[string]any) (toolAnswer, error) {
	raw, err := c.CallTool(ctx, tool, args)
	var te *mcpclient.ToolError
	switch {
	case errors.As(err, &te):
		return toolAnswer{refused: true, code: te.Code, message: te.Message, text: te.Error()}, nil
	case err != nil:
		return toolAnswer{}, err
	}
	return toolAnswer{text: string(raw)}, nil
}

// runMCP calls every probe as B's agent: each must be refused, the refusal
// must be the same for A's object as for a ghost, and nothing returned may
// hold a marker of A. It returns the calls made and, per tool, the codes
// they were refused with.
func (w *world) runMCP() (int, []string, error) {
	ctx, cancel := context.WithTimeout(stopCtx, 2*time.Minute)
	defer cancel()
	c := mcpclient.New(w.d.baseURL, w.b.agent.token)
	w.h.called(http.MethodPost, "/mcp")
	if err := c.Initialize(ctx, "isolation-check"); err != nil {
		return 0, nil, fmt.Errorf("MCP initialize as B's agent: %w", err)
	}
	var failures, codes []string
	n := 0
	for _, p := range mcpProbes() {
		s := p.pick(w)
		a := asked{ids: []kindID{{s.kind, s.id}}}
		if s.name != "" {
			a.strs = []string{s.name}
		}
		got, err := callTool(ctx, c, p.tool, p.args(w, s))
		n++
		if err != nil {
			failures = append(failures, fmt.Sprintf("  %s about A's %s: %v", p.tool, s.kind, err))
			continue
		}
		if leaks := w.marks.leaks([]byte(got.text), a); len(leaks) > 0 {
			failures = append(failures, fmt.Sprintf("  %s about A's %s: the answer holds %s", p.tool, s.kind, strings.Join(leaks, "; ")))
			continue
		}
		if !got.refused || got.code == "" {
			failures = append(failures, fmt.Sprintf("  %s about A's %s: answered %s, want a refusal with a code", p.tool, s.kind, w.h.redact(got.text)))
			continue
		}
		if extra := numbersBeyond(got.message, a); len(extra) > 0 {
			failures = append(failures, fmt.Sprintf("  %s about A's %s: the refusal %q holds numbers %v", p.tool, s.kind, got.message, extra))
			continue
		}
		codes = append(codes, p.tool+" "+got.code)
		g := w.ghost()
		g.kind = s.kind
		ghost, err := callTool(ctx, c, p.tool, p.args(w, g))
		n++
		if err != nil {
			failures = append(failures, fmt.Sprintf("  %s about a ghost: %v", p.tool, err))
			continue
		}
		if !ghost.refused || ghost.code != got.code || string(normalize([]byte(ghost.message), g)) != string(normalize([]byte(got.message), s)) {
			failures = append(failures, fmt.Sprintf("  %s: about A's %s %q %q, about one that never existed %q %q: the difference tells B that A's exists",
				p.tool, s.kind, got.code, got.message, ghost.code, ghost.message))
		}
	}
	if len(failures) > 0 {
		return n, codes, fmt.Errorf("%d of the MCP probes failed:\n%s", len(failures), strings.Join(failures, "\n"))
	}
	return n, codes, nil
}

// agentSession opens B's agent's own session in B's first channel.
func (w *world) openAgentSession() error {
	var sess schema.SessionV1
	if err := w.b.agent.api.call(http.MethodPost, "/v1/sessions", schema.CreateSessionRequestV1{Client: w.bName("agent"),
		Interventions: []schema.InterventionKind{schema.InterventionKindMessage}}, &sess); err != nil {
		return err
	}
	w.b.agentSession = sess.ID
	return nil
}
