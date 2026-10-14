// Copy of the approval object in pkg/schema/approval.go, option.go and
// state.go. A message carries one as the data of a payload named
// conch.approval.v1.
import { array, int, object, optional, str, timestamp, vocab } from './validate';
import { payload, type Payload } from './message';

export const approvalPayloadSchema = 'conch.approval.v1';

export type OptionKind = 'approve' | 'reject' | 'custom';

export interface Option {
  id: string;
  label: string;
  kind: OptionKind;
}

export const option = object<Option>({
  id: str,
  label: str,
  kind: vocab<OptionKind>({ approve: true, reject: true, custom: true }),
});

export type ApprovalState = 'pending' | 'escalated' | 'resolved' | 'expired';

export const approvalState = vocab<ApprovalState>({ pending: true, escalated: true, resolved: true, expired: true });

export type EscalationTargetKind = 'principal' | 'ntfy_topic';

// Exactly one of principal_id and topic is set, chosen by kind.
export interface EscalationTarget {
  kind: EscalationTargetKind;
  principal_id?: number;
  topic?: string;
}

export const escalationTarget = object<EscalationTarget>({
  kind: vocab<EscalationTargetKind>({ principal: true, ntfy_topic: true }),
  principal_id: optional(int),
  topic: optional(str),
});

export interface ApprovalV1 {
  id: number;
  requester_id: number;
  channel_id: number;
  title: string;
  body: string;
  payload?: Payload;
  options: Option[];
  deadline: string;
  quorum: number;
  escalation_target?: EscalationTarget;
  state: ApprovalState;
  created_at: string;
}

export const approvalV1 = object<ApprovalV1>({
  id: int,
  requester_id: int,
  channel_id: int,
  title: str,
  body: str,
  payload: optional(payload),
  options: array(option),
  deadline: timestamp,
  quorum: int,
  escalation_target: optional(escalationTarget),
  state: approvalState,
  created_at: timestamp,
});
