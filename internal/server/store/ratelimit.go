package store

// AuditAgentRateLimited is written when an agent's call is refused because it
// has used its manifest rate limit (issue #80): for the 1st, 2nd, 4th, 8th, ...
// refusal of a principal and capability within a minute, never once per
// refused call.
const AuditAgentRateLimited = "agent_rate_limited"
