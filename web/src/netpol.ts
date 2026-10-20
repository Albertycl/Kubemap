// The "Can A reach B?" form as data, and its query (KM-36 UI); free of React
// and the DOM so node --test can check it.

export type SourceKind = "pod" | "ip";
export type DestKind = "service" | "pod" | "ip";

export interface CheckForm {
  srcKind: SourceKind;
  srcNamespace: string;
  srcPod: string;
  srcIp: string;
  dstKind: DestKind;
  dstNamespace: string;
  dstName: string; // pod or Service
  dstIp: string;
  port: string;
  protocol: string;
}

export const emptyForm: CheckForm = {
  srcKind: "pod", srcNamespace: "", srcPod: "", srcIp: "",
  dstKind: "service", dstNamespace: "", dstName: "", dstIp: "",
  port: "", protocol: "TCP",
};

/**
 * The query for GET .../netpol/check, or the first thing the form still
 * needs, in plain words.
 */
export function checkQuery(f: CheckForm): { params?: URLSearchParams; missing?: string } {
  const p = new URLSearchParams();
  if (f.srcKind === "pod") {
    if (!f.srcNamespace || !f.srcPod) return { missing: "Choose the source pod." };
    p.set("fromNamespace", f.srcNamespace);
    p.set("fromPod", f.srcPod);
  } else {
    if (!f.srcIp.trim()) return { missing: "Enter the source IP address." };
    p.set("fromIp", f.srcIp.trim());
  }
  if (f.dstKind === "ip") {
    if (!f.dstIp.trim()) return { missing: "Enter the destination IP address." };
    p.set("toIp", f.dstIp.trim());
  } else {
    if (!f.dstNamespace || !f.dstName) return { missing: f.dstKind === "service" ? "Choose the destination Service." : "Choose the destination pod." };
    p.set("toNamespace", f.dstNamespace);
    p.set(f.dstKind === "service" ? "toService" : "toPod", f.dstName);
  }
  const port = Number(f.port);
  if (!Number.isInteger(port) || port < 1 || port > 65535) return { missing: "Enter a port from 1 to 65535." };
  p.set("port", String(port));
  p.set("protocol", f.protocol || "TCP");
  return { params: p };
}

/** Allowed and blocked counts of a matrix; a Service with no pods behind it is neither. */
export function matrixCounts(cells: { allowed: boolean; noDestination?: boolean; error?: string }[][]): {
  allowed: number;
  blocked: number;
  empty: number;
  errors: number;
} {
  let allowed = 0, blocked = 0, empty = 0, errors = 0;
  for (const row of cells)
    for (const c of row) {
      if (c.error) errors++;
      else if (c.noDestination) empty++;
      else if (c.allowed) allowed++;
      else blocked++;
    }
  return { allowed, blocked, empty, errors };
}
