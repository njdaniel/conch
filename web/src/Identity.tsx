import { identityMarks, identityText, type IdentityInput, type IdentityKey } from './identityMarks';

// markIds gives a mark an element id, so another element can refer to it (a
// profile card's aria-describedby names the row's You badge, #2071).
export function IdentityMarks({ only, textForm = false, markIds, ...input }: IdentityInput & { readonly only?: readonly IdentityKey[]; readonly textForm?: boolean; readonly markIds?: Partial<Record<IdentityKey, string>> }) {
  return identityMarks(input)
    .filter((mark) => only === undefined || only.includes(mark.key))
    .map((mark) => (
      <span key={mark.key} id={markIds?.[mark.key]} className={`ag-badge identity-mark identity-${mark.key}`} data-identity={mark.key} data-text={identityText(mark)}>
        <span className="vh"> [</span>
        <span className="identity-label">{textForm ? mark.key : mark.label}</span>
        <span className="vh">]</span>
      </span>
    ));
}
