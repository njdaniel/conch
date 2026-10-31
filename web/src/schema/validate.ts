// The combinators the wire types' validators are built from. pkg/schema is
// the source of every wire shape (CLAUDE.md rule 2): cmd/schema-ts writes each
// type and its validator into generated.ts from the Go types, and
// schema.test.ts checks each against the Go golden fixtures through them.
//
// A validator is tied to its type by the compiler: object<T>() must be given a
// validator for every key of T, of that key's type, and vocab<T>() every member
// of T. So a type and its validator cannot drift apart without the typecheck
// failing, and a type that has drifted from Go fails the fixture test.
//
// Every combinator is marked @__NO_SIDE_EFFECTS__: building a validator does
// nothing but return a function, so the bundler may drop a generated validator
// nobody imports. That is what keeps the desktop owner's voice-control types
// (issue #948) out of the browser build; scripts/desktop-web-dist-check.sh
// fails if they are ever in it.

// Validator<T> checks that a decoded JSON value has the shape of T. It returns
// the problems it found, each starting with where in the value it is; an empty
// list means the value matches.
export interface Validator<T> {
  (value: unknown, path?: string): string[];
  // Never set. It makes Validator<string> and Validator<number> different
  // types, so a validator cannot stand in for another type's.
  readonly type?: T;
}

// Optional marks a key the Go side omits when empty (omitempty).
export interface Optional<T> {
  readonly optional: Validator<T>;
}

type OptionalKeys<T> = {
  // eslint-disable-next-line @typescript-eslint/no-empty-object-type -- {} is how a conditional type asks whether a key may be absent
  [K in keyof T]-?: {} extends Pick<T, K> ? K : never;
}[keyof T];

// Shape<T> is what object<T>() needs: one validator per key of T, wrapped in
// Optional exactly when the key is optional in T.
export type Shape<T> = {
  readonly [K in keyof T]-?: K extends OptionalKeys<T>
    ? Optional<Exclude<T[K], undefined>>
    : Validator<T[K]>;
};

/* @__NO_SIDE_EFFECTS__ */
function validator<T>(check: (value: unknown, path: string) => string[]): Validator<T> {
  return (value: unknown, path = '$') => check(value, path);
}

function describe(value: unknown): string {
  if (value === null) return 'null';
  if (Array.isArray(value)) return 'an array';
  return `a ${typeof value}`;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

export const str: Validator<string> = validator((v, path) =>
  typeof v === 'string' ? [] : [`${path}: want a string, got ${describe(v)}`],
);

// int is an integer id or count: Go's int64, which JSON carries as a number.
export const int: Validator<number> = validator((v, path) =>
  typeof v === 'number' && Number.isSafeInteger(v)
    ? []
    : [`${path}: want an integer, got ${typeof v === 'number' ? String(v) : describe(v)}`],
);

export const bool: Validator<boolean> = validator((v, path) =>
  typeof v === 'boolean' ? [] : [`${path}: want a boolean, got ${describe(v)}`],
);

// RFC 3339 in UTC or with an offset, as Go's time.Time and schema.Timestamp
// write it.
const rfc3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/;

export const timestamp: Validator<string> = validator((v, path) =>
  typeof v === 'string' && rfc3339.test(v) && !Number.isNaN(Date.parse(v))
    ? []
    : [`${path}: want an RFC 3339 timestamp, got ${typeof v === 'string' ? JSON.stringify(v) : describe(v)}`],
);

// json accepts any JSON value: a payload's data, which Go keeps opaque.
export const json: Validator<unknown> = validator((v, path) =>
  v === undefined ? [`${path}: want a JSON value`] : [],
);

// vocab checks a closed vocabulary. Its argument must name every member of T
// and nothing else, so the list cannot fall behind the type.
/* @__NO_SIDE_EFFECTS__ */
export function vocab<T extends string>(members: { readonly [V in T]: true }): Validator<T> {
  const allowed = Object.keys(members);
  return validator((v, path) =>
    typeof v === 'string' && allowed.includes(v)
      ? []
      : [`${path}: want one of ${allowed.join(', ')}, got ${typeof v === 'string' ? JSON.stringify(v) : describe(v)}`],
  );
}

/* @__NO_SIDE_EFFECTS__ */
export function array<T>(item: Validator<T>): Validator<T[]> {
  return validator((v, path) =>
    Array.isArray(v)
      ? v.flatMap((element, i) => item(element, `${path}[${String(i)}]`))
      : [`${path}: want an array, got ${describe(v)}`],
  );
}

/* @__NO_SIDE_EFFECTS__ */
export function optional<T>(v: Validator<T>): Optional<T> {
  return { optional: v };
}

// object checks a JSON object against T: every required key present, every key
// of the right type, and no key T does not have. A key the fixture has and the
// type lacks means the type is behind Go.
/* @__NO_SIDE_EFFECTS__ */
export function object<T>(shape: Shape<T>): Validator<T> {
  const fields: [string, Validator<unknown> | Optional<unknown>][] = Object.entries(shape);
  return validator((v, path) => {
    if (!isRecord(v)) {
      return [`${path}: want an object, got ${describe(v)}`];
    }
    const problems: string[] = [];
    for (const [key, field] of fields) {
      const at = `${path}.${key}`;
      if ('optional' in field) {
        if (key in v) problems.push(...field.optional(v[key], at));
      } else if (key in v) {
        problems.push(...field(v[key], at));
      } else {
        problems.push(`${at}: required key is missing`);
      }
    }
    for (const key of Object.keys(v)) {
      if (!Object.hasOwn(shape, key)) {
        problems.push(`${path}.${key}: key is not in the TypeScript type`);
      }
    }
    return problems;
  });
}

// tagged checks a union discriminated by key, such as Audience by kind. It
// needs one validator per value of the key; a value with none is refused, so
// an unknown variant is never read as a known one.
/* @__NO_SIDE_EFFECTS__ */
export function tagged<T extends Record<K, string>, K extends string>(
  key: K,
  cases: { readonly [V in T[K]]: Validator<Extract<T, Record<K, V>>> },
): Validator<T> {
  const byTag = new Map<string, Validator<unknown>>(Object.entries(cases));
  return validator((v, path) => {
    if (!isRecord(v)) {
      return [`${path}: want an object, got ${describe(v)}`];
    }
    const tag = v[key];
    const check = typeof tag === 'string' ? byTag.get(tag) : undefined;
    if (check === undefined) {
      return [
        `${path}.${key}: want one of ${[...byTag.keys()].join(', ')}, got ${typeof tag === 'string' ? JSON.stringify(tag) : describe(tag)}`,
      ];
    }
    return check(v, path);
  });
}
