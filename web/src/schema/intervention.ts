// Copies of pkg/schema/intervention.go.
import { array, int, object, optional, str, timestamp, vocab } from './validate';

// InterventionKind is what a person can ask of a run. This client sends
// message and cancel only; pause and resume are in the vocabulary because a
// session may declare them.
export type InterventionKind = 'message' | 'cancel' | 'pause' | 'resume';

export const interventionKind = vocab<InterventionKind>({ message: true, cancel: true, pause: true, resume: true });

export type InterventionState = 'requested' | 'acknowledged' | 'applied' | 'failed';

export const interventionState = vocab<InterventionState>({
  requested: true,
  acknowledged: true,
  applied: true,
  failed: true,
});

export const interventionSchema = 'conch.intervention.v1';

// InterventionV1 is one ask of a run and the agent's answer to it.
export interface InterventionV1 {
  schema: typeof interventionSchema;
  id: number;
  run_id: number;
  kind: InterventionKind;
  body?: string;
  state: InterventionState;
  requested_by: number;
  requested_at: string;
  answered_at?: string;
  reason?: string;
}

export const interventionV1 = object<InterventionV1>({
  schema: vocab<typeof interventionSchema>({ [interventionSchema]: true }),
  id: int,
  run_id: int,
  kind: interventionKind,
  body: optional(str),
  state: interventionState,
  requested_by: int,
  requested_at: timestamp,
  answered_at: optional(timestamp),
  reason: optional(str),
});

// CreateInterventionRequestV1 is the body of POST /v1/runs/{id}/interventions.
export interface CreateInterventionRequestV1 {
  kind: InterventionKind;
  body?: string;
}

export const createInterventionRequestV1 = object<CreateInterventionRequestV1>({
  kind: interventionKind,
  body: optional(str),
});

export interface ListInterventionsResponseV1 {
  interventions: InterventionV1[];
}

export const listInterventionsResponseV1 = object<ListInterventionsResponseV1>({
  interventions: array(interventionV1),
});
