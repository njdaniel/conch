import { identityMarks, identityText, type IdentityInput, type IdentityKey } from './identityMarks';

export function IdentityMarks({ only, textForm = false, ...input }: IdentityInput & { readonly only?: readonly IdentityKey[]; readonly textForm?: boolean }) {
  return identityMarks(input)
    .filter((mark) => only === undefined || only.includes(mark.key))
    .map((mark) => (
      <span key={mark.key} className={`ag-badge identity-mark identity-${mark.key}`} data-identity={mark.key} data-text={identityText(mark)}>
        <span className="vh"> [</span>
        <span className="identity-label">{textForm ? mark.key : mark.label}</span>
        <span className="vh">]</span>
      </span>
    ));
}
