import { useState, type FormEvent } from "react";
import { api, ApiError, type Me } from "./api";
import { totpLoginError } from "./totp";

function errorText(e: unknown): string {
  if (e instanceof ApiError) {
    const t = totpLoginError(e.code);
    if (t) return t;
    if (e.code === "bad_credentials") return "Wrong username or password.";
    if (e.code === "too_many_attempts") return "Too many failed attempts. Wait 15 minutes and try again.";
    return e.message;
  }
  return "Cannot reach the portal.";
}

export function Login({ onLogin }: { onLogin: (me: Me) => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [needCode, setNeedCode] = useState(false); // the server asked for the second factor
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      onLogin(await api.login(username, password, code.trim()));
    } catch (err) {
      if (err instanceof ApiError && err.code === "totp_required") setNeedCode(true);
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="center">
      <form className="card auth" onSubmit={submit}>
        <h1>
          <span className="logo" aria-hidden="true">
            ◈
          </span>{" "}
          Kubemap
        </h1>
        <label>
          Username
          <input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" required />
        </label>
        <label>
          Password
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
            autoFocus
            required
          />
        </label>
        <label>
          Authenticator code {needCode ? "" : "(only if you turned on two-factor login)"}
          <input
            value={code}
            onChange={(e) => setCode(e.target.value)}
            autoComplete="one-time-code"
            inputMode="text"
            placeholder="123456 or a recovery code"
            autoFocus={needCode}
          />
        </label>
        {error && <p className="error" role="alert">{error}</p>}
        <button className="primary" disabled={busy}>
          {busy ? "Logging in…" : "Log in"}
        </button>
        <p className="muted small">
          First time? Run <code>kubectl -n kubemap-system exec deploy/kubemap -- kubemap-portal reset-admin-password</code> to
          get the first password for the administrator account. You change it at the first login.
        </p>
      </form>
    </div>
  );
}

const MIN = 12;
const MAX_BYTES = 72;

export function ChangePassword({ me, onDone }: { me: Me; onDone: (me: Me) => void }) {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [repeat, setRepeat] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const bytes = new TextEncoder().encode(next).length;
  const problem =
    next.length > 0 && [...next].length < MIN
      ? `At least ${MIN} characters.`
      : bytes > MAX_BYTES
        ? `At most ${MAX_BYTES} bytes.`
        : repeat.length > 0 && repeat !== next
          ? "The two new passwords differ."
          : "";

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (problem || next !== repeat) return;
    setBusy(true);
    setError("");
    try {
      onDone(await api.changePassword(current, next));
    } catch (err) {
      setError(err instanceof ApiError && err.code === "bad_credentials" ? "The current password is wrong." : errorText(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="center">
      <form className="card auth" onSubmit={submit}>
        <h1>Set a new password</h1>
        <p className="muted">
          {me.mustChangePassword
            ? `Before you continue, replace the first password of "${me.username}".`
            : "Choose a new password."}
        </p>
        <label>
          Current password
          <input type="password" value={current} onChange={(e) => setCurrent(e.target.value)} autoComplete="current-password" autoFocus required />
        </label>
        <label>
          New password
          <input type="password" value={next} onChange={(e) => setNext(e.target.value)} autoComplete="new-password" required />
        </label>
        <label>
          New password again
          <input type="password" value={repeat} onChange={(e) => setRepeat(e.target.value)} autoComplete="new-password" required />
        </label>
        {(problem || error) && (
          <p className="error" role="alert">
            {problem || error}
          </p>
        )}
        <button className="primary" disabled={busy || !!problem || !next || next !== repeat}>
          {busy ? "Saving…" : "Save password"}
        </button>
      </form>
    </div>
  );
}
