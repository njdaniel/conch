# Design: the generic webhook/event adapter

- **Status:** Adopted 2026-10-14 under Nick's delegation ("skip waiting on me, use your recommendations"); §11 lists each decision that is his to overrule. Children: #386 (F10a), #387 (F10b), #388 (F10c). F10a (#386) has landed; F10b (#387) is the relay mode of §7, in this PR (`internal/adapter/relay*.go`).
- **Touches:** [ADR-000](../adr/ADR-000-charter.md) D3 (deployment invariant), D6 (API parity), D8 (schema-first), D10 (manifests); [ADR-001](../adr/ADR-001-mcp-native-architecture.md); the P3 epic #263 decisions D1 (sessions), D5 (reference adapter), D8 (run audience), D9 (channel-less agent calls), D10 (who may intervene).
- **Owner:** protocol-designer (this note), server-engineer (the children)
- **Issue:** #359 (tracking)

The P3 "Ships" list names a generic webhook/event adapter beside the subprocess adapter: something for a runtime that cannot be run as a subprocess but can send HTTP about its own work. Examples are a CI job, a hosted agent, or a script on another machine. Issue #359 asks four questions: where it lives, how it authenticates, what it does about interventions, and what shape it accepts. This note answers them.

The main finding comes first, because it changes the question. **`conchd` already accepts the runtime-neutral contract over HTTP from a runtime outside Conch.** The session, run, activity and intervention routes added by #265, #266 and #287 take exactly the `pkg/schema` request types. They authenticate an agent's issued credential in the `Authorization` header. They are held to the agent's manifest, its rate limits and the audit log. A runtime that can set a header and keep an id between two calls can report into Conch today with `curl`. So the work left is the case those routes do not cover, a sender that cannot hold a session, plus documentation and a test proving the rest. It is not a new server ingress.

## 1. What already exists

| Route (REST) | MCP tool | Body in → out | Gate |
|---|---|---|---|
| `POST /v1/sessions` | `register_session` | `CreateSessionRequestV1` → `SessionV1` | `sessions.manage` (own-session check, D9) |
| `POST /v1/sessions/{id}/heartbeat` | `heartbeat` | none → `SessionV1` | `sessions.manage` |
| `DELETE /v1/sessions/{id}` | `end_session` | none → 204 | `sessions.manage` |
| `POST /v1/sessions/{id}/runs` | `start_run` | `CreateRunRequestV1` → `RunV1` | `runs.manage`, membership, and `post` (or `post_net`, `whisper`) in the run's channel |
| `POST /v1/runs/{id}/state` | `update_run` | `SetRunStateRequestV1` → `RunV1` | as above, re-checked on every report |
| `POST /v1/runs/{id}/activity` | `post_activity` | `AppendActivityRequestV1` → `AgentActivityV1` | as above |
| `GET /v1/sessions/{id}/interventions` | `poll_interventions` | none → `ListInterventionsResponseV1` | `runs.manage` (own-session check) |
| `POST /v1/interventions/{id}/ack` | `ack_intervention` | `AckInterventionRequestV1` → `InterventionV1` | `runs.manage` and the run's channel gate |

All of these are on `conchd` with no external process (D3). Each route and its tool call the same function (`internal/server/calls.go`), so a request is allowed or refused alike on both fronts (D6). Every body is decoded strictly by `decodeJSONBody`: at most `schema.MaxPostRequestBytes` (1 MiB), unknown fields refused, one JSON object only. It is then checked by the type's own `Validate`.

The webhook ingest, `POST /v1/hooks/{token}` in `internal/server/hooks.go`, is a different thing. The issue cites it as `internal/server/webhook*.go`, but no file has that name. It is an operator-created capability URL bound to one channel and one principal. It posts a message and nothing else: it touches no session, run or activity. Its token is in the path, not a header, and the route is `accessExempt` in the route table. §4 compares the two.

## 2. Who sends, and what each can do

| Sender | Sets an `Authorization` header | Keeps a Conch id between calls | Has a process for the life of the run | What serves it |
|---|---|---|---|---|
| A script or service on another machine | yes | yes | yes | The REST routes in §1, as they are. |
| A CI job: steps that run as separate commands | yes | yes, in a file or the job's environment | yes, the job | The REST routes and a heartbeat loop in the background (§6), or the relay (§7). |
| A runtime that only reports at step boundaries, from many short commands | yes | yes | no single command lives as long as the run | The relay (§7), started as a sidecar for the run's life. |
| A hosted service firing its own webhooks (GitHub `workflow_job`, GitLab pipeline events and so on) | no: fixed headers, HMAC-signed | no: it ignores the response | no | Nothing in this note (§12). It needs translation from a vendor's shape. |

The last row decides question 1. A route that took the neutral contract with a URL token would be for senders that can shape a body but cannot set a header. The only common senders like that are vendors' webhooks. They cannot shape the body either. They also never read a response, so they could not learn the run id that every later report needs. Such a route would serve no sender that the existing routes do not already serve.

## 3. Question 1: where it lives

**Decision: no new route and no new authentication class on `conchd`. The generic event ingress is the existing REST contract (§1). The one piece left to build is a second mode of `conch-adapter`, the relay (§7). It runs as an optional separate process beside the runtime and holds the session for a sender that cannot.**

Reasons:

- **Rule 1 (D3) holds either way, but the attack surface does not.** New `/v1/hooks/...` routes would need no external process. They would still be a second way into the run tables, with weaker authentication than the first, a path token instead of a header credential. This proposal adds nothing to `conchd`'s routes, its route table or its authentication.
- **Rule 4 (D6).** The relay is a client of the public API. It can do nothing the REST routes cannot, so no capability exists only behind it.
- **Statefulness belongs to a client.** The gap is keeping a session alive and remembering ids. Doing that inside `conchd` for a remote sender would mean storing sender state server-side, keyed on something the schema does not have (§6). A process beside the runtime does it with the existing types.
- **The subprocess adapter is the precedent.** D5 made `conch-adapter` the place where runtimes meet the neutral contract. The relay is the same program in the other direction. It owns the session and the heartbeat as the subprocess mode does, and it takes its runs from a local socket instead of from a command it started.

## 4. Question 2: authentication and authorization

**Decision: every sender, the relay included, authenticates to `conchd` with an issued agent credential of an agent principal of its own. Its manifest grants exactly `sessions.manage`, `runs.manage` and `post` in the one channel it reports to, and declares a rate limit on both capabilities. Nothing on `conchd` changes. The relay's own listener is a Unix socket only (§7).**

The issue asks that the new path be bounded, rate limited and audited like the webhook ingest. The existing routes already do each of those things, and some more strictly:

| | Webhook ingest (`hooks.go`) | Run and session routes (§1) | The relay's socket (§7) |
|---|---|---|---|
| Who authenticates | Whoever holds the URL; the token is stored only as its SHA-256 | An issued credential in the header; never in a URL; a static `--mcp-token` is not accepted (REST: 401, since it is wired only to `/mcp`; MCP: `sessions_require_auth`) | Whoever can open the socket: its owner's uid (mode 0600 in a 0700 directory) |
| With `--auth off` | Posts, with no membership or manifest check | Refused: `sessions_require_auth` | Cannot start: `conchd` refuses its session |
| Disabled principal | 404 `hook_not_found` | 401: the credential no longer resolves; its sessions are ended (`principal_disabled`) | `conchd`'s 401, passed back |
| Membership | Required (agent or human principal); refusal looks like an unknown token | Required; refusal is 404 `channel_not_found`, audited `access_denied reason=not_a_member` | `conchd`'s refusal; the relay also refuses any channel but its own, with the same 404 |
| Manifest | `messages.post` with `post`, if the principal is an agent | `runs.manage` with the audience's permission, on create and again on every report; `sessions.manage` for the session | `conchd`'s decision |
| Rate limit | `messages.post` budget of the agent, if declared; 429 `Retry-After` | `runs.manage` and `sessions.manage` budgets, if declared; 429 `Retry-After`, audited `agent_rate_limited` (thinned) | `conchd`'s 429 and `Retry-After`, passed back |
| Body | 1 MiB; `text/plain` or a strict v1 post | 1 MiB; strict decode; the type's `Validate` | 1 MiB; strict decode with the same types before anything is forwarded |
| Author or owner | Always the hook's principal | Always the credential's principal; a run belongs to the credential that owns its session | The relay's credential |

Two points need a word:

- **Rate limits are opt-in.** A manifest with no `rate_limits` entry for a capability leaves it unlimited (`AgentManifestV1.RateLimits`). For an external reporter the documented manifest must declare both, and the test in child C1 uses one that does. A reporter heartbeating every 10 seconds spends 6 `sessions.manage` a minute. A CI job reports a few dozen `runs.manage` calls per run. §9 covers what is still unbounded.
- **One principal and one credential per external runtime.** A shared credential shares its 16 live sessions (`store.MaxLiveSessionsPerCredential`) and its budget. A dedicated one can be expired (`--expires-at`) and revoked alone. Revoking it ends its sessions (`session_ended reason=credential_revoked`). The docs say so.

This is security-sensitive: the children that touch it get a `security-reviewer` pass (§10).

## 5. Question 3: interventions

**Decision: an external runtime declares no interventions unless it polls. It declares exactly the kinds it will poll for and answer. The relay declares none by default.**

An external runtime cannot be signalled: `conchd` makes no outbound call to it, which keeps every call in one direction as the voice note chose (voice-control-plane §6). The roadmap rule that unsupported operations are absent then decides the rest:

- **A direct sender** lists kinds in `CreateSessionRequestV1.Interventions` only if it polls `GET /v1/sessions/{id}/interventions?state=requested` (the issue's `list_interventions` is `poll_interventions` over MCP). It answers each one through `POST /v1/interventions/{id}/ack`. A kind it does not list is refused at once with 409 `intervention_unsupported` and audited. The TUI and browser do not offer it (UX-19, P3-08).
- **The relay** declares the kinds set in `CONCH_ADAPTER_INTERVENTIONS`, empty by default. It passes the poll and the answer through its socket (§7). It never answers on the runtime's behalf except in one case: an intervention the runtime has not answered within `CONCH_ADAPTER_INTERVENTION_TIMEOUT` (default 60 s) is answered `failed` with the reason `not taken by <runtime> within 60s`. A person who asks gets an answer. A late answer from the runtime then gets `conchd`'s 409 `invalid_transition`, passed back.
- **Answering never moves the run** (#287). After applying a cancel, the runtime reports the run `cancelled` itself.
- A `message` intervention's text reaches the external runtime as input. Whoever can see the run can send it (D10). The runtime's owner must treat that text as they would a chat message, as the README says for the subprocess adapter.

## 6. Question 4: the shape

**Decision: no new type and no new field. A sender uses exactly the types in §1, plus `Error` for refusals. The relay accepts and returns the same types.**

A CI job maps onto the closed vocabularies without strain:

| CI event | Call | Value |
|---|---|---|
| Job picked up | start run | `task`: one line; `link`: the job's https URL; no `message_id` unless a chat message started the job |
| Job begins | state | `starting`, then `running`; activity `run_started` |
| A step starts and ends | activity | `command_started` and `command_finished`, summary = the step's name, outcome = its result |
| Job waits on a manual gate | state | `waiting`, then `running` |
| Job ends | state, activity | `completed`, `failed` or `cancelled` with a one-line `outcome`; activity `run_ended` |

Things deliberately not added. Each would change a published request type, so it would go through the `schema-change` skill: a new request field is additive on the wire, but because conchd decodes strictly, an older server would refuse a request carrying it. Each is recorded so it is not reinvented by a handler:

- **A sender's timestamp on activity.** `at` is the server's clock (`AppendActivityRequestV1`). A sender that replays a backlog after an outage gets the times of receipt, not of the events.
- **An idempotency key.** Retries are the sender's problem. A retried start creates a second run, and a retried activity is a second entry. A retried state change after a lost response gets 409 `invalid_transition`, which the sender reads as already done. Senders must not retry a start blindly.
- **An external id or correlation key.** The sender keeps the run id `conchd` returned. A sender that cannot is the relay's case, or §12's.
- **A per-session heartbeat lease.** Liveness stays as D1 has it: a heartbeat or an open events socket, against the server-wide thresholds (30 s stale, 90 s offline by default). A report on a run does **not** keep its session alive. A sparse sender that does not heartbeat shows `stale` and then `offline`, and its open run reads `outcome_unknown` (P3-07). That is accurate: Conch has heard nothing. The documented recipe runs a heartbeat loop in the background. The relay heartbeats for the life of its process.
- **Vendor payloads.** No shape of a third party's becomes a `pkg/schema` type.

## 7. The relay mode of `conch-adapter`

`CONCH_ADAPTER_MODE=relay` (the default stays `subprocess`). This is the specification child C2 builds; nothing here is a new wire shape.

**Lifecycle.** On start the relay registers one session on `conchd`. The session has client `conch-adapter`, the runtime label from `CONCH_ADAPTER_RUNTIME` (a free label in this mode, with no profile behind it), host, and the declared interventions. It heartbeats it every `CONCH_ADAPTER_HEARTBEAT` (10 s) until it stops. It reads no channel and starts no command, so its manifest needs no `messages.read` or `messages.post`. On SIGTERM or SIGINT it ends the session and leaves open runs as they are. Unlike the subprocess mode, which cancels a run because it killed the command, the relay does not know how an external run ended. It does not invent an outcome. The run stays in its last reported state with no outcome; because an ended session leaves the roster, `outcome_unknown` is not shown for it. A relay that dies instead of stopping (no `DELETE`) leaves its session to go stale and offline, and then the roster shows `outcome_unknown` on the open run (P3-15 asserts both).

**The socket.** A Unix domain socket only, at `CONCH_ADAPTER_RELAY_SOCKET`, by default `$XDG_RUNTIME_DIR/conch-adapter-relay.sock`. If neither is set, the relay refuses to start. The relay creates the socket mode 0600, in a directory that must be mode 0700 and owned by its uid. It refuses a path that exists and is not a socket. It removes a stale socket only while it holds its lock file. There is no TCP listener and no shared secret. A sender that cannot reach a local socket calls `conchd`'s REST routes itself. On a platform without Unix sockets, relay mode refuses to start.

**What the socket serves.** These are `conchd`'s routes with the session made implicit. The bodies are the `pkg/schema` types, decoded strictly at 1 MiB, and the relay forwards them over REST. It does not use MCP: MCP re-encodes `post_activity`'s `data`, which sorts its keys and turns its numbers into float64. `conchd`'s status, `Error` body and `Retry-After` header are passed back unchanged.

| On the socket | Forwarded to |
|---|---|
| `GET /v1/session` | none; the relay's `SessionV1` as of its last heartbeat (a readiness check for a script) |
| `POST /v1/runs` | `POST /v1/sessions/{its session}/runs` |
| `POST /v1/runs/{id}/state` | the same path |
| `POST /v1/runs/{id}/activity` | the same path |
| `GET /v1/interventions?state=&after=&limit=` | `GET /v1/sessions/{its session}/interventions` |
| `POST /v1/interventions/{id}/ack` | the same path |

**Its own refusals.** Each uses an existing code. A run id the relay did not create is 404 `run_not_found`. Two relays on one credential could otherwise report on each other's runs, which `conchd` cannot tell apart, since it checks the credential and not the session. An intervention id the relay has not returned from a poll is 404 `intervention_not_found`. A channel other than `CONCH_ADAPTER_CHANNEL` is 404 `channel_not_found`. The relay also returns 413 `request_too_large` and 400 `invalid_request`. None of these reaches `conchd`, so none is in its audit log. The relay logs one line per refusal with the code, never a body.

## 8. Threat model

**Who can call.** On `conchd`, nobody new: the holders of an issued agent credential, on the routes they could already call, within their memberships and manifest. On the relay, any process running as the relay's uid, or as root. In a CI job that means every step, including third-party actions, which can already read the job's secrets.

**What they can make Conch show.** Everything below appears only in the channels the principal is a member of and has `post` in. It reaches each member's TUI and browser live through the events socket.

- A session in the roster: its `client` and its `model`, shown to every member; its `runtime` and `host`, shown to operators and the agent only. All four follow the display-name rule: no control characters, no bidi overrides, no invisible marks.
- Runs: a one-line `task` (512 characters), an https `link` with no userinfo, and an `outcome` (512).
- Activity: a one-line `summary`, an `outcome`, and `data` of up to 8 KiB, shown as data.
- Answers to interventions: a `reason` of prose with no control characters but newline and tab.

They cannot post or read messages without `messages.post` and `messages.read`, which the documented manifest does not grant. They cannot raise approvals, attach or fetch files (agents are refused on every file route), choose an author, back-date anything, or reach a channel they are not a member of. Two things they can do are worth saying:

- **A reporter can choose labels that imitate another runtime**, for example `model: claude-opus`. The principal's name and id are the operator's and are shown beside every session and run. Readers should trust those, not the labels.
- **A reporter can link a run to any channel-wide message** it can see, by `message_id`. The run then appears as an answer to that message. Every agent can do this today. Reporters started by a chat command want it; the docs tell other reporters to omit it.

**Abuse limits that exist.**

- 16 live sessions per credential.
- 16 waiting interventions per run.
- 1 MiB per request, and the field limits above.
- The manifest's per-capability rate limits, when declared.
- The events socket: a slow subscriber is closed at 256 queued references, and no writer waits for one.
- Revoking a credential ends its sessions at once.

**What is unbounded today** (§9): runs per session, open runs per session, activity entries per run, and session churn (a reporter looping register and `DELETE` writes two audit rows and a roster update to every member's socket per cycle, bounded only by an opt-in `sessions.manage` limit). An external reporter is the likeliest source of a runaway loop, which makes these worth closing (child C3).

**What reaches the audit log.**

- `session_started` and `session_ended` (with the reason: client, credential revoked, principal disabled, credential expired), and `session_offline`.
- `run_started` and `run_ended`. The detail names the session, principal, channel and message, never the task.
- `intervention_requested`, `intervention_answered` and `intervention_unsupported`.
- Every `access_denied`, with the capability and the reason, and every `agent_rate_limited`, both thinned to the first eight a minute and then the 16th, 32nd and so on.
- Credential issue, rotation and revocation.

Not in the log:

- Heartbeats, and state changes before a run ends.
- Activity entries. Those are stored as the run's own record and served under the run's visibility.
- Anything the relay refuses before `conchd` sees it.
- No token, credential or socket path is ever written to the log.

**Compromise.** A stolen reporter credential can show misleading runs, in its granted channels, up to its rate limit, until it is revoked or expires; it can also answer a person's intervention `applied` without acting on it, a false assurance that a run stopped; and a run's `link` is any `https://` host, rendered to members as a link, so a forged run can carry a phishing link. It cannot do anything else in Conch. Tokens cross the network in the clear unless the operator puts TLS in front of `conchd`, as for every client (README, Known limitations).

## 9. What the code says that the issue assumed otherwise

1. The routes the issue proposes building already exist (§1). The issue's option of "new routes on `conchd` that accept the neutral contract directly" describes `/v1/sessions`, `/v1/runs` and `/v1/sessions/{id}/interventions` as they are.
2. `internal/server/webhook*.go` does not exist. The ingest is `internal/server/hooks.go`, with its store in `internal/server/store/hooks.go`.
3. The webhook ingest does not use an agent credential and manifest the way the issue's question 2 implies. It uses a path token bound to a channel and a principal, and it checks the manifest only when that principal is an agent and authentication is required. It is rate limited only when the agent's manifest declares a `messages.post` limit. A hook bound to a human principal has no rate limit at all.
4. The tool is `poll_interventions`, not `list_interventions`.
5. A run report does not count as a heartbeat. A sender that only reports goes stale after 30 seconds.
6. Nothing bounds the number of runs per session, open runs per session or activity entries per run. Only the optional `runs.manage` rate limit stands in the way. This holds for every agent, not only external ones.

None of these conflicts with a locked decision in ADR-000.

## 10. Phasing

The child issues are filed: #386 (F10a), #387 (F10b), #388 (F10c).

1. **C1, S: document and test reporting from outside over the REST contract.** A README recipe and a REST-only end-to-end test of the happy path and every refusal. No change to the server. This alone answers the issue for every sender that can set a header.
2. **C2, M: the relay mode (§7).** Security-reviewer pass. Blocked by C1.
3. **C3, M: server-side bounds on open runs per session and activity per run.** Two error codes, added through `schema-change`. Security-reviewer pass. It applies to every agent and is independent of C1 and C2.

## 11. Decisions that are Nick's

Nick said to use the recommendations rather than wait. Each item below is his to confirm or reverse, and each has a recommendation.

1. **No new ingress on `conchd`.** The `/v1/hooks/...` neutral-contract option is declined. The existing REST routes are the event ingress. *Recommended:* accept. It adds no route, no authentication class and no security-sensitive server change.
2. **A new local ingress in an optional process.** This is `conch-adapter` relay mode on an owner-only Unix socket, with no TCP and no listener reachable from the internet. *Recommended:* accept, with a security-reviewer pass on C2.
3. **No new `pkg/schema` type or field.** The additions in §6 are declined: sender timestamps, idempotency keys, external ids and a heartbeat lease. *Recommended:* accept. Any of them can come back later through `schema-change`.
4. **The roadmap item is met by C1 and C2.** Translating vendors' webhooks, and any internet-facing listener, move out of P3 into a later issue with its own design note. This narrows a "Ships" item. *Recommended:* accept, and edit ROADMAP.md's #359 line when C2 merges.
5. **New limits for every agent (C3):** 32 open runs per session and 10,000 activity entries per run. Over either, a call is refused with 429 and a new error code, `too_many_runs` or `too_much_activity`. Calls accepted today would be refused. *Recommended:* accept those values.

Taken here under the delegation, recorded so they can be reopened:

- Interventions are declared none by default.
- The relay answers `failed` after 60 s for an intervention its runtime has not taken.
- The relay never invents a run's state.
- The relay forwards over REST, not MCP.
- The relay serves one channel.
- Each external runtime gets one principal and one credential.

## 12. Out of scope

- **Vendors' webhooks.** No translation of GitHub, GitLab or Buildkite payloads, no HMAC verification, no mapping or template language from someone else's JSON to the neutral contract. A vendor translator would be a profile of the relay with its own design, an internet-facing listener, and its own security review.
- **Anything new on `conchd`.** No route, no authentication class, and no URL token for runs or sessions.
- **Conch calling out.** No outbound webhook to the runtime and no delivery of an intervention by callback.
- **Pause and resume semantics** (#345, #346). The relay passes on whatever kinds it declares; it acts on none.
- **Running without authentication.** With `--auth off`, sessions and runs are unavailable, and so is all of this.
- **Messages from an external runtime.** A reporter that also posts uses the existing hook or `messages.post`, under the rules it has today.
