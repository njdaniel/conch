// Copies of pkg/schema/activity.go.
import { array, int, json, object, optional, str, timestamp, vocab } from './validate';

export type ActivityKind =
  | 'run_started'
  | 'command_started'
  | 'command_finished'
  | 'reply_posted'
  | 'approval_requested'
  | 'approval_resolved'
  | 'intervention_received'
  | 'run_ended';

export const activityKind = vocab<ActivityKind>({
  run_started: true,
  command_started: true,
  command_finished: true,
  reply_posted: true,
  approval_requested: true,
  approval_resolved: true,
  intervention_received: true,
  run_ended: true,
});

export const activitySchema = 'conch.agent.activity.v1';

// AgentActivityV1 is one entry of a run's activity log. data is kept opaque
// and the page never shows it.
export interface AgentActivityV1 {
  schema: typeof activitySchema;
  id: number;
  run_id: number;
  principal_id: number;
  kind: ActivityKind;
  summary: string;
  outcome?: string;
  at: string;
  data?: unknown;
}

export const agentActivityV1 = object<AgentActivityV1>({
  schema: vocab<typeof activitySchema>({ [activitySchema]: true }),
  id: int,
  run_id: int,
  principal_id: int,
  kind: activityKind,
  summary: str,
  outcome: optional(str),
  at: timestamp,
  data: optional(json),
});

export interface ListActivityResponseV1 {
  activity: AgentActivityV1[];
  next_after?: number;
}

export const listActivityResponseV1 = object<ListActivityResponseV1>({
  activity: array(agentActivityV1),
  next_after: optional(int),
});
