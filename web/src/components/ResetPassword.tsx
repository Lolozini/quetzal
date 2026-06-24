import { FormEvent, useState } from "react";
import { api, ApiError } from "../api";
import { useT } from "../i18n";

// ResetPassword is shown when the app loads with a #reset=<token> link from a
// password-reset email. On success it returns to the login screen.
export function ResetPassword({ token, onDone }: { token: string; onDone: () => void }) {
  const { t } = useT();
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");
  const [done, setDone] = useState(false);
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError("");
    if (password.length < 8) {
      setError(t("Password must be at least 8 characters."));
      return;
    }
    if (password !== confirm) {
      setError(t("Passwords don't match."));
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
            <p className="muted">{t("Your password has been reset.")}</p>
            <button type="button" className="primary" style={{ marginTop: 16, width: "100%" }} onClick={onDone}>
              {t("Back to sign in")}
            </button>
          </>
        ) : (
          <>
            <p className="muted">{t("Choose a new password")}</p>
            <label>{t("New password")}</label>
            <input
              type="password"
              autoComplete="new-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoFocus
            />
            <label>{t("Confirm password")}</label>
            <input
              type="password"
              autoComplete="new-password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
            />
            {error && <div className="error">{error}</div>}
            <button className="primary" style={{ marginTop: 16, width: "100%" }} disabled={busy}>
              {busy ? "…" : t("Reset password")}
            </button>
            <button
              type="button"
              className="link"
              style={{ marginTop: 8, width: "100%" }}
              onClick={onDone}
            >
              {t("Cancel")}
            </button>
          </>
        )}
      </form>
    </div>
  );
}
