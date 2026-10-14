// Copies of pkg/schema/principal.go and pkg/schema/whoami.go.
import { int, object, str, vocab } from './validate';

// PrincipalKind: a person or an agent (ADR-000 D1).
export type PrincipalKind = 'human' | 'agent';

export const principalKind = vocab<PrincipalKind>({ human: true, agent: true });

// PrincipalRole: operators administer the instance, members use it.
export type PrincipalRole = 'operator' | 'member';

export const principalRole = vocab<PrincipalRole>({ operator: true, member: true });

// WhoAmIResponseV1 is the body of GET /v1/whoami.
export interface WhoAmIResponseV1 {
  id: number;
  kind: PrincipalKind;
  name: string;
  role: PrincipalRole;
}

export const whoAmIResponseV1 = object<WhoAmIResponseV1>({
  id: int,
  kind: principalKind,
  name: str,
  role: principalRole,
});
