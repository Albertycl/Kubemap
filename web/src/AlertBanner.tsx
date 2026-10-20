import { useState } from "react";
import { alertApi, type Alert } from "./api";
import { usePoll } from "./common";
import { alertText, criticalCount, loadClosed, saveClosed, visibleAlerts } from "./alerts";

const POLL_MS = 30000;
const storage = (): Storage | undefined => {
  try {
    return window.localStorage;
  } catch {
    return undefined;
  }
};

/** The alerts, shared by the banner and the badges of one page. */
export function useAlerts(): Alert[] | undefined {
  return usePoll(alertApi.list, POLL_MS).data;
}

/**
 * The banner under the top bar: every cluster with a critical finding now
 * (KM-31). Closing one hides it until its findings change.
 */
export function AlertBanner() {
  const alerts = useAlerts();
  const [closed, setClosed] = useState<Set<string>>(() => loadClosed(storage()));
  const shown = alerts ? visibleAlerts(alerts, closed) : [];

  if (shown.length === 0) return null;
  const close = (a: Alert) => {
    const next = new Set(closed).add(a.key);
    setClosed(next);
    saveClosed(storage(), next, alerts ?? []);
  };
  return (
    <div className="alert-banner" role="alert">
      {shown.map((a) => (
        <div key={a.key} className="alert-row">
          <span className="alert-dot" aria-hidden="true" />
          <strong>Critical</strong>
          <span className="alert-text">{alertText(a)}</span>
          <span className="muted small">since {new Date(a.since).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</span>
          <span className="spacer" />
          <a href={`#/clusters/${encodeURIComponent(a.clusterId)}/problems`}>Show problems</a>
          <a href="#/journal">Draft incident</a>
          <button className="link" onClick={() => close(a)} aria-label={`Close the alert for ${a.clusterName}`}>
            ✕
          </button>
        </div>
      ))}
    </div>
  );
}

/** The cluster badge: how many critical findings the cluster has now. */
export function CriticalBadge({ alerts, clusterId }: { alerts: Alert[] | undefined; clusterId: string }) {
  const n = criticalCount(alerts, clusterId);
  if (n === 0) return null;
  return (
    <a className="badge level-fail critical-badge" href={`#/clusters/${encodeURIComponent(clusterId)}/problems`} title="Critical findings now">
      {n} critical
    </a>
  );
}
