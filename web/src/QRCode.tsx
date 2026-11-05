// A QR code as an SVG of <rect> elements with presentation attributes only
// (conchd's style-src 'self' refuses a style attribute). The label says what
// the picture is for, never what it holds: the text is a secret.
import type { ReactElement } from 'react';
import type { QR } from './qr';

const quiet = 4;

export function QRCode({ code, label }: { readonly code: QR; readonly label: string }) {
  const side = code.modules.length + 2 * quiet;
  const rects: ReactElement[] = [];
  code.modules.forEach((row, y) => { row.forEach((dark, x) => {
      if (dark) rects.push(<rect key={`${String(x)}-${String(y)}`} x={x + quiet} y={y + quiet} width={1} height={1} fill="#000" />);
    }); },
  );
  return (
    <svg className="qr" viewBox={`0 0 ${String(side)} ${String(side)}`} role="img" aria-label={label} shapeRendering="crispEdges">
      <rect x={0} y={0} width={side} height={side} fill="#fff" />
      {rects}
    </svg>
  );
}
