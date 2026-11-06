import { linkify } from './linkify';

// Linkified renders a message body as text, with each http(s) URL in it as an
// anchor. Every place a body is shown uses this one component.
export function Linkified({ text }: { readonly text: string }) {
  return (
    <>
      {linkify(text).map((segment, i) =>
        segment.kind === 'text' ? (
          segment.text
        ) : (
          <a key={i} href={segment.href} target="_blank" rel="noopener noreferrer">
            {segment.text}
          </a>
        ),
      )}
    </>
  );
}
