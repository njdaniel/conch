// The page's own words for a name the server's rules refuse (issue #1082).
// The server's message (ValidateDisplayName in pkg/schema/name.go, and
// nameInvisibleMessage in internal/server/principals.go) names the rule but
// speaks of a "principal name" with a "schema:" prefix, which a person
// choosing a name should never read. It is matched on the rule's words, not
// the whole string, so a reworded message still gets a plain line.

export const nameUnusable = 'That name cannot be used. Choose another.';

const rules: readonly (readonly [RegExp, string])[] = [
  [/invisible/i, 'A name cannot contain invisible characters.'],
  [/begin or end with whitespace/i, 'A name cannot start or end with a space.'],
  [/control characters|bidi controls/i, 'A name cannot contain control characters.'],
  [/at most \d+ characters/i, 'A name can be up to 100 characters.'],
  [/reserved/i, 'Names that begin with “guest” are reserved.'],
  // The account routes' own answer to a name of nothing a reader can see
  // (issue #441), such as one that is only U+3164.
  [/character a reader can see|^the name is not acceptable$/i, 'A name needs at least one character you can see.'],
];

// isNameMessage says whether a server message is about the name.
export function isNameMessage(message: string | undefined): boolean {
  return message !== undefined && /\bnames?\b/i.test(message);
}

// nameRefusalLine is the plain line for a refused name's server message.
export function nameRefusalLine(message: string | undefined): string {
  const text = message ?? '';
  for (const [pattern, line] of rules) if (pattern.test(text)) return line;
  return nameUnusable;
}
