import { useCallback, useEffect, useState, type FormEvent } from "react";
import { api, type TotpSetup, type TotpStatus } from "./api";
import { CopyBox, errorMessage } from "./common";
import { QrCode } from "./QrCode";
import { groupSecret, looksLikeCode, recoveryText, totpStatusText } from "./totp";

/** Account page: turn two-factor (TOTP) login on or off. */
export function SecurityPage({ onChanged }: { onChanged: () => void }) {
  const [status, setStatus] = useState<TotpStatus | null>(null);
  const [setup, setSetup] = useState<TotpSetup | null>(null);
  const [codes, setCodes] = useState<string[] | null>(null);
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(() => {
    api
      .totpStatus()
      .then(setStatus)
      .catch((e) => setError(errorMessage(e)));
  }, []);
  useEffect(load, [load]);

  async function run(f: () => Promise<void>) {
    setBusy(true);
    setError("");
    try {
      await f();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  const start = (e: FormEvent) => {
    e.preventDefault();
    void run(async () => {
      setSetup(await api.totpSetup(password));
      setPassword("");
    });
  };
  const confirm = (e: FormEvent) => {
    e.preventDefault();
    void run(async () => {
      const r = await api.totpEnable(code);
      setCodes(r.recoveryCodes);
      setSetup(null);
      setCode("");
      load();
      onChanged();
    });
  };
  const turnOff = (e: FormEvent) => {
    e.preventDefault();
    void run(async () => {
      await api.totpDisable(password, code);
      setPassword("");
      setCode("");
      setCodes(null);
      load();
      onChanged();
    });
  };

  return (
    <>
      <div className="pagehead">
        <h1>Account security</h1>
      </div>
      <div className="card">
        <h2>Two-factor login (authenticator app)</h2>
        {status ? <p>{totpStatusText(status.enabled, status.recoveryCodesLeft)}</p> : <p className="muted">Loading…</p>}
        {error && (
          <p className="error" role="alert">
            {error}
          </p>
        )}

        {codes && (
          <div>
            <p>
              <strong>Save these 8 recovery codes now.</strong> Each works once if you lose your phone. They are not shown again.
            </p>
            <CopyBox text={recoveryText(codes)} label="recovery codes" />
            <button className="link" onClick={() => setCodes(null)}>
              I saved them
            </button>
          </div>
        )}

        {status && !status.enabled && !setup && (
          <form onSubmit={start} className="auth" style={{ display: "grid", gap: "0.7rem", maxWidth: "380px" }}>
            <label>
              Your password
              <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required />
            </label>
            <button className="primary" disabled={busy || !password}>
              Set up two-factor login
            </button>
          </form>
        )}

        {setup && (
          <form onSubmit={confirm} style={{ display: "grid", gap: "0.7rem", maxWidth: "560px" }}>
            <p>
              1. In your authenticator app, scan this QR code with your phone (or add an account by key, or open the link below on
              your phone). Type of key: time-based, 6 digits, 30 seconds.
            </p>
            <QrCode text={setup.uri} label="QR code for the authenticator app" />
            <p>
              Secret: <code className="wrap">{groupSecret(setup.secret)}</code>
            </p>
            <CopyBox text={setup.uri} label="otpauth link" />
            <p>2. Type the 6-digit code the app shows now:</p>
            <label>
              Code
              <input value={code} onChange={(e) => setCode(e.target.value)} inputMode="numeric" autoComplete="one-time-code" autoFocus required />
            </label>
            <button className="primary" disabled={busy || !looksLikeCode(code)}>
              Turn on
            </button>
          </form>
        )}

        {status?.enabled && (
          <form onSubmit={turnOff} style={{ display: "grid", gap: "0.7rem", maxWidth: "380px", marginTop: "1rem" }}>
            <h3>Turn off</h3>
            <label>
              Your password
              <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required />
            </label>
            <label>
              Code (or a recovery code)
              <input value={code} onChange={(e) => setCode(e.target.value)} autoComplete="one-time-code" required />
            </label>
            <button className="primary" disabled={busy || !password || !looksLikeCode(code)}>
              Turn off two-factor login
            </button>
          </form>
        )}
      </div>
    </>
  );
}
