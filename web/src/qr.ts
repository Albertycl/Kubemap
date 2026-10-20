// QR code helpers for the two-factor setup page. Pure functions, so they can
// be tested without a browser. The code is drawn in the browser: the secret
// is never sent to another site. Run: npm test.
import qrcode from "qrcode-generator";

/** The QR modules of `text` (true = dark), error correction level M. */
export function qrMatrix(text: string): boolean[][] {
  const qr = qrcode(0, "M");
  qr.addData(text);
  qr.make();
  const n = qr.getModuleCount();
  return Array.from({ length: n }, (_, r) => Array.from({ length: n }, (_, c) => qr.isDark(r, c)));
}

/** One SVG path for all dark modules (1 unit per module). */
export function qrPath(matrix: boolean[][]): string {
  const parts: string[] = [];
  matrix.forEach((row, r) => {
    row.forEach((dark, c) => {
      if (dark) parts.push(`M${c} ${r}h1v1h-1z`);
    });
  });
  return parts.join("");
}
