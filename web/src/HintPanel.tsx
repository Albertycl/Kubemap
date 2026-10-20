import { useEffect, useState } from "react";
import { hintsApi } from "./api";
import { CopyBox, errorMessage } from "./common";
import type { Hint, HintSignals } from "./hints";

/**
 * "What does this mean?": loads the diagnosis hints for one object when it is
 * opened. Read-only: the commands are text to copy, nothing is run here.
 */
export function HintPanel({ signals }: { signals: HintSignals }) {
  const [open, setOpen] = useState(false);
  const [hints, setHints] = useState<Hint[] | undefined>();
  const [error, setError] = useState("");
  const key = JSON.stringify(signals);
  useEffect(() => {
    if (!open) return;
    let live = true;
    hintsApi
      .get(signals)
      .then((r) => live && setHints(r.hints))
      .catch((e) => live && setError(errorMessage(e)));
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, key]);
  return (
    <details className="hint-panel" onToggle={(e) => setOpen((e.target as HTMLDetailsElement).open)} onClick={(e) => e.stopPropagation()}>
      <summary>What does this mean?</summary>
      {error && <p className="error small">{error}</p>}
      {!error && !hints && open && <p className="muted small">Loading…</p>}
      {hints?.length === 0 && <p className="muted small">No hint for this status yet.</p>}
      {hints?.map((h) => (
        <div key={h.slug} className="hint">
          <p>{h.meaning}</p>
          <h4>Check (read-only, copy and run yourself)</h4>
          {h.check.map((c) => (
            <CopyBox key={c} text={c} label={`Command: ${c}`} />
          ))}
          {h.causes.length > 0 && (
            <>
              <h4>Likely causes</h4>
              <ul>
                {h.causes.map((c) => (
                  <li key={c}>{c}</li>
                ))}
              </ul>
            </>
          )}
          <p className="muted small">Source notes: {h.notes.join("; ")}</p>
        </div>
      ))}
    </details>
  );
}
