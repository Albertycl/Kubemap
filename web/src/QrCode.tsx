import { useMemo } from "react";
import { qrMatrix, qrPath } from "./qr";

const QUIET = 4; // blank border, in modules, that scanners need

/** A QR code drawn as SVG. Always black on white, also in dark mode, so phones can read it. */
export function QrCode({ text, label }: { text: string; label: string }) {
  const matrix = useMemo(() => qrMatrix(text), [text]);
  const size = matrix.length + QUIET * 2;
  return (
    <svg
      role="img"
      aria-label={label}
      viewBox={`${-QUIET} ${-QUIET} ${size} ${size}`}
      width={220}
      height={220}
      shapeRendering="crispEdges"
      style={{ background: "#fff", borderRadius: 6, border: "1px solid #d0d7de", maxWidth: "100%" }}
    >
      <rect x={-QUIET} y={-QUIET} width={size} height={size} fill="#fff" />
      <path d={qrPath(matrix)} fill="#000" />
    </svg>
  );
}
