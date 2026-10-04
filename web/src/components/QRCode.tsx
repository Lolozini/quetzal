import { useMemo } from "react";
import { encode } from "uqr";

// QRCode draws value as a QR code, in the page itself: what it encodes -- a
// two-factor secret -- never leaves the browser for an image service. It is
// black on white whatever the theme, inside the four-module margin the
// standard asks for: many scanners read neither an inverted code nor one
// without its margin.
export function QRCode({ value, label, size = 200 }: { value: string; label: string; size?: number }) {
  const { modules, path } = useMemo(() => {
    const qr = encode(value, { ecc: "M", border: 4 });
    let d = "";
    qr.data.forEach((row, y) =>
      row.forEach((dark, x) => {
        if (dark) d += `M${x},${y}h1v1h-1z`;
      }),
    );
    return { modules: qr.size, path: d };
  }, [value]);
  return (
    <svg
      className="qr"
      role="img"
      aria-label={label}
      viewBox={`0 0 ${modules} ${modules}`}
      width={size}
      height={size}
      shapeRendering="crispEdges"
    >
      <rect width={modules} height={modules} fill="#fff" />
      <path d={path} fill="#000" />
    </svg>
  );
}
