import { FormEvent, useState } from "react";
import { api, ApiError } from "../api";

// ResetPassword is shown when the app loads with a ?reset=<token> link from a
// password-reset email. On success it returns to the login screen.
export function ResetPassword({ token, onDone }: { token: string; onDone: () => void }) {
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");
  const [done, setDone] = useState(false);
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError("");
    if (password.length < 8) {
      setError("Password must be at least 8 characters.");
      return;
    }
    if (password !== confirm) {
      setError("Passwords don't match.");
      return;
    }
    setBusy(true);
    try {
      await api.resetPassword(token, password);
      setDone(true);
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
        {done ? (
          <>
            <p className="muted">Your password has been reset.</p>
            <button type="button" className="primary" style={{ marginTop: 16, width: "100%" }} onClick={onDone}>
              Back to sign in
            </button>
          </>
        ) : (
          <>
            <p className="muted">Choose a new password</p>
            <label>New password</label>
            <input
              type="password"
              autoComplete="new-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoFocus
            />
            <label>Confirm password</label>
            <input
              type="password"
              autoComplete="new-password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
            />
            {error && <div className="error">{error}</div>}
            <button className="primary" style={{ marginTop: 16, width: "100%" }} disabled={busy}>
              {busy ? "…" : "Reset password"}
            </button>
            <button
              type="button"
              className="link"
              style={{ marginTop: 8, width: "100%" }}
              onClick={onDone}
            >
              Cancel
            </button>
          </>
        )}
      </form>
    </div>
  );
}
