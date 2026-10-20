import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, statusText, type Cluster } from "./api";

/**
 * Polls load() every intervalMs while the tab is visible. When key changes
 * (for example a selected time range) it loads again at once.
 */
export function usePoll<T>(load: () => Promise<T>, intervalMs = 5000, key?: string) {
  const [data, setData] = useState<T | undefined>(undefined);
  const [error, setError] = useState<unknown>(null);
  const loadRef = useRef(load);
  loadRef.current = load;
  // Only the latest request may set the state: an older answer that arrives
  // late (for example after the key changed) is dropped.
  const seq = useRef(0);

  const refresh = useCallback(async () => {
    const mine = ++seq.current;
    try {
      const d = await loadRef.current();
      if (mine !== seq.current) return;
      setData(d);
      setError(null);
    } catch (e) {
      if (mine !== seq.current) return;
      setError(e);
    }
  }, []);

  useEffect(() => {
    refresh();
    const t = window.setInterval(() => {
      if (document.visibilityState === "visible") refresh();
    }, intervalMs);
    return () => window.clearInterval(t);
  }, [refresh, intervalMs, key]);

  return { data, error, refresh };
}

export function errorMessage(e: unknown): string {
  if (e instanceof ApiError) return e.message;
  return "Cannot reach the portal.";
}

export function StatusBadge({ cluster }: { cluster: Cluster }) {
  return (
    <span className={`badge ${cluster.status}`}>
      <span className="dot" aria-hidden="true" />
      {statusText(cluster)}
    </span>
  );
}

/** A read-only block with a Copy button. */
export function CopyBox({ text, label }: { text: string; label: string }) {
  const [state, setState] = useState<"idle" | "copied" | "selected">("idle");
  const preRef = useRef<HTMLPreElement>(null);
  // The "Copied" label resets after 2.5 s; the timer is cleared if the box
  // goes away first (review km-20-fixes N-b).
  const timer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(timer.current), []);
  async function copy() {
    try {
      await navigator.clipboard.writeText(text);
      setState("copied");
    } catch {
      // Some browsers refuse the clipboard (for example on a certificate
      // warning page). Select the text instead, so Ctrl+C works.
      const pre = preRef.current;
      const sel = window.getSelection();
      if (pre && sel) {
        const range = document.createRange();
        range.selectNodeContents(pre);
        sel.removeAllRanges();
        sel.addRange(range);
      }
      setState("selected");
    }
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setState("idle"), 2500);
  }
  return (
    <div className="copybox">
      <pre ref={preRef} aria-label={label}>
        {text}
      </pre>
      <button type="button" onClick={copy}>
        {state === "copied" ? "Copied" : state === "selected" ? "Press Ctrl+C" : "Copy"}
      </button>
    </div>
  );
}
