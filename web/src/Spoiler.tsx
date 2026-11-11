import { useId, useState, type ReactNode } from 'react';

// One reveal control for text (`||spoiler||`) and for attachments. A native
// button with aria-expanded and aria-controls; the content is absent from the
// DOM until revealed, so nothing hidden is decoded, fetched or announced.
//
// Uncontrolled by default. An attachment passes `revealed` and `onToggle` so
// the reveal can start its fetch and Hide can revoke what it made.
export function Spoiler({
  children,
  revealed: controlled,
  onToggle,
  showText = 'Reveal spoiler',
  hideText = 'Hide spoiler',
  label,
  block = false,
}: {
  readonly children: ReactNode;
  readonly revealed?: boolean;
  readonly onToggle?: (revealed: boolean) => void;
  readonly showText?: string;
  readonly hideText?: string;
  // Names what the button reveals, for a screen reader: "Reveal photo.png".
  readonly label?: string;
  readonly block?: boolean;
}) {
  const [own, setOwn] = useState(false);
  const id = useId();
  const revealed = controlled ?? own;
  const text = revealed ? hideText : showText;
  const Tag = block ? 'div' : 'span';
  return (
    <Tag className={revealed ? 'spoiler revealed' : 'spoiler'}>
      <button
        type="button"
        aria-expanded={revealed}
        aria-controls={id}
        {...(label === undefined ? {} : { 'aria-label': `${text} ${label}` })}
        onClick={() => {
          if (controlled === undefined) setOwn(!revealed);
          onToggle?.(!revealed);
        }}
      >
        {text}
      </button>
      {revealed ? <span id={id} className="spoiler-content">{children}</span> : null}
    </Tag>
  );
}
