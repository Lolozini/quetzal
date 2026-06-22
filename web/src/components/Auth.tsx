import { FormEvent, useState } from "react";
import { api, ApiError, User } from "../api";

export function Auth({
  setupNeeded,
  onAuthed,
}: {
  setupNeeded: boolean;
  onAuthed: (u: User) => void;
}) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [twoFactor, setTwoFactor] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      if (setupNeeded) {
        onAuthed(await api.setup(username, password));
        return;
      }
      const res = await api.login(username, password, twoFactor ? code : undefined);
      if ("twoFactorRequired" in res) {
        setTwoFactor(true); // ask for the code, keep username/password
      } else {
        onAuthed(res);
      }
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="center">
      <form className="card" style={{ width: 360 }} onSubmit={submit}>
        <h1>
          Quetz<span style={{ color: "var(--accent)" }}>al</span>
        </h1>
        <p className="muted">
          {setupNeeded
            ? "Create the admin account"
            : twoFactor
              ? "Enter your authentication code"
              : "Sign in to continue"}
        </p>
        {!twoFactor && (
          <>
            <label>Username</label>
            <input value={username} onChange={(e) => setUsername(e.target.value)} autoFocus />
            <label>Password</label>
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </>
        )}
        {twoFactor && (
          <>
            <label>Authentication code</label>
            <input
              value={code}
              onChange={(e) => setCode(e.target.value)}
              autoFocus
              autoComplete="one-time-code"
              inputMode="text"
              placeholder="6-digit code or recovery code"
            />
            <p className="muted">From your authenticator app, or a recovery code.</p>
          </>
        )}
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 16, width: "100%" }} disabled={busy}>
          {busy ? "…" : setupNeeded ? "Create admin" : twoFactor ? "Verify" : "Sign in"}
        </button>
      </form>
    </div>
  );
}
