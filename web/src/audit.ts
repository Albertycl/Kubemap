// Helpers for the audit log page, free of React and the DOM so node --test
// can check them. Kubemap is read-only by design, so the log holds logins,
// account changes and journal edits only.

/** Audit log filter groups by action name. */
export type AuditGroup = "all" | "journal" | "login";

export function auditGroup(action: string): Exclude<AuditGroup, "all"> | "other" {
  if (action.startsWith("journal.")) return "journal";
  if (["login", "logout", "password.change", "password.reset", "reauth"].includes(action) || action.startsWith("password")) return "login";
  return "other";
}

/** An audit result's level for colour. */
export function resultLevel(result: string): "ok" | "warn" | "fail" | "info" {
  if (result === "ok" || result.startsWith("ok:")) return "ok";
  if (result.startsWith("refused") || result === "failed" || result.startsWith("failed")) return "fail";
  if (result.startsWith("unknown")) return "warn";
  return "info";
}
