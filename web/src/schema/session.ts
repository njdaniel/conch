// Copies of pkg/schema/session.go.
import { array, bool, int, object, optional, str, timestamp, vocab } from './validate';
import { principalKind, type PrincipalKind } from './principal';
import { interventionKind, type InterventionKind } from './intervention';

// SessionState is stored liveness; SessionStatus is what a roster shows,
// derived by the server from the state and the session's runs.
export type SessionState = 'online' | 'stale' | 'offline';

export const sessionState = vocab<SessionState>({ online: true, stale: true, offline: true });

export type SessionStatus = 'online' | 'idle' | 'running' | 'blocked' | 'waiting' | 'failed' | 'stale' | 'offline';

export const sessionStatus = vocab<SessionStatus>({
  online: true,
  idle: true,
  running: true,
  blocked: true,
  waiting: true,
  failed: true,
  stale: true,
  offline: true,
});

export const sessionSchema = 'conch.session.v1';
export const sessionRosterSchema = 'conch.session_roster.v1';

// SessionV1 is one live client or runtime connected as a principal. The
// agent-only fields are absent on a human's.
export interface SessionV1 {
  schema: typeof sessionSchema;
  id: number;
  principal_id: number;
  kind: PrincipalKind;
  name: string;
  client: string;
  state: SessionState;
  status: SessionStatus;
  started_at: string;
  last_seen_at: string;
  ended_at?: string;
  runtime?: string;
  model?: string;
  host?: string;
  interventions: InterventionKind[];
  current_run?: number;
  task?: string;
  outcome_unknown: boolean;
}

export const sessionV1 = object<SessionV1>({
  schema: vocab<typeof sessionSchema>({ [sessionSchema]: true }),
  id: int,
  principal_id: int,
  kind: principalKind,
  name: str,
  client: str,
  state: sessionState,
  status: sessionStatus,
  started_at: timestamp,
  last_seen_at: timestamp,
  ended_at: optional(timestamp),
  runtime: optional(str),
  model: optional(str),
  host: optional(str),
  interventions: array(interventionKind),
  current_run: optional(int),
  task: optional(str),
  outcome_unknown: bool,
});

// SessionRosterV1 is the whole set of sessions visible in one channel.
export interface SessionRosterV1 {
  schema: typeof sessionRosterSchema;
  channel_id: number;
  sessions: SessionV1[];
}

export const sessionRosterV1 = object<SessionRosterV1>({
  schema: vocab<typeof sessionRosterSchema>({ [sessionRosterSchema]: true }),
  channel_id: int,
  sessions: array(sessionV1),
});

// CreateSessionRequestV1 is the body of POST /v1/sessions.
export interface CreateSessionRequestV1 {
  client: string;
  runtime?: string;
  model?: string;
  host?: string;
  interventions?: InterventionKind[];
}

export const createSessionRequestV1 = object<CreateSessionRequestV1>({
  client: str,
  runtime: optional(str),
  model: optional(str),
  host: optional(str),
  interventions: optional(array(interventionKind)),
});
