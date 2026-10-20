// Helpers for the two-factor (TOTP) login and the security page. Pure
// functions, so they can be tested without a browser. Run: npm test.

/** Drops spaces and dashes and upper-cases a typed code ("abcd-efgh" -> "ABCDEFGH"). */
export function normalizeCode(s: string): string {
  return s.replace(/[\s-]+/g, "").toUpperCase();
}

/** True for a 6-digit authenticator code (after normalizing). */
export function isTotpCode(s: string): boolean {
  return /^\d{6}$/.test(normalizeCode(s));
}

/** True for something that can be submitted as a code: 6 digits or a 12-character recovery code. */
export function looksLikeCode(s: string): boolean {
  const c = normalizeCode(s);
  return /^\d{6}$/.test(c) || /^[A-Z2-7]{12}$/.test(c);
}

/** The secret in groups of four, easier to type: "ABCD EFGH ...". */
export function groupSecret(secret: string): string {
  return secret.replace(/\s+/g, "").replace(/(.{4})(?=.)/g, "$1 ");
}

/** Text for a login error. Returns undefined for codes this file does not know. */
export function totpLoginError(code: string | undefined): string | undefined {
  switch (code) {
    case "totp_required":
      return "Enter the 6-digit code from your authenticator app (or a recovery code).";
    case "bad_totp":
      return "Wrong or already used code. Wait for the next code and try again.";
    default:
      return undefined;
  }
}

/** One line about the state of two-factor login. */
export function totpStatusText(enabled: boolean, recoveryCodesLeft: number): string {
  if (!enabled) return "Two-factor login is off.";
  const rc = recoveryCodesLeft === 1 ? "1 recovery code left" : `${recoveryCodesLeft} recovery codes left`;
  return recoveryCodesLeft <= 2 ? `Two-factor login is on. Only ${rc}: turn it off and on again to get new codes.` : `Two-factor login is on (${rc}).`;
}

/** Recovery codes as text to copy or print, one per line. */
export function recoveryText(codes: string[]): string {
  return codes.join("\n");
}
