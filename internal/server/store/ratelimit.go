package store

// AuditAgentRateLimited is written when an agent's call is refused because it
// has used its manifest rate limit (issue #80): for each of the first eight
// refusals of a principal and capability within a minute and then the 16th,
// 32nd, ..., never once per refused call without bound.
const AuditAgentRateLimited = "agent_rate_limited"
