import { FormEvent, useState } from "react";
import { api, ApiError, User } from "../api";
import { LangSwitcher, useT } from "../i18n";

export function Auth({
  setupNeeded,
  onAuthed,
}: {
  setupNeeded: boolean;
  onAuthed: (u: User) => void;
}) {
  const { t } = useT();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [email, setEmail] = useState("");
  const [code, setCode] = useState("");
  const [twoFactor, setTwoFactor] = useState(false);
  const [forgot, setForgot] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      if (setupNeeded) {
        onAuthed(await api.setup(username, password, email.trim() || undefined));
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

  if (forgot) {
    return <Forgot onBack={() => setForgot(false)} />;
  }

  return (
    <div className="center">
      <form className="card" style={{ width: 360 }} onSubmit={submit}>
        <h1>
          Quetz<span style={{ color: "var(--accent)" }}>al</span>
        </h1>
        <p className="muted">
          {setupNeeded
            ? t("Create the admin account")
            : twoFactor
              ? t("Enter your authentication code")
              : t("Sign in to continue")}
        </p>
        {!twoFactor && (
          <>
            <label>{t("Username")}</label>
            <input value={username} onChange={(e) => setUsername(e.target.value)} autoFocus />
            <label>{t("Password")}</label>
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
            {setupNeeded && (
              <>
                <label>{t("Email (optional, for password reset)")}</label>
                <input
                  type="email"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  placeholder="you@example.com"
                />
              </>
            )}
          </>
        )}
        {twoFactor && (
          <>
            <label>{t("Authentication code")}</label>
            <input
              value={code}
              onChange={(e) => setCode(e.target.value)}
              autoFocus
              autoComplete="one-time-code"
              inputMode="text"
              placeholder={t("6-digit code or recovery code")}
            />
            <p className="muted">{t("From your authenticator app, or a recovery code.")}</p>
          </>
        )}
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 16, width: "100%" }} disabled={busy}>
          {busy ? "…" : setupNeeded ? t("Create admin") : twoFactor ? t("Verify") : t("Sign in")}
        </button>
        {!setupNeeded && !twoFactor && (
          <button type="button" className="link" style={{ marginTop: 8, width: "100%" }} onClick={() => setForgot(true)}>
            {t("Forgot password?")}
          </button>
        )}
        <div className="row" style={{ marginTop: 12, justifyContent: "center" }}>
          <LangSwitcher />
        </div>
      </form>
    </div>
  );
}

// Forgot asks for an identifier and requests a reset email. The response is
// intentionally uniform (it never reveals whether the account exists).
function Forgot({ onBack }: { onBack: () => void }) {
  const { t } = useT();
  const [identifier, setIdentifier] = useState("");
  const [sent, setSent] = useState(false);
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      await api.forgotPassword(identifier.trim());
    } catch {
      /* uniform response: ignore errors */
    } finally {
      setBusy(false);
      setSent(true);
    }
  }

  return (
    <div className="center">
      <form className="card" style={{ width: 360 }} onSubmit={submit}>
        <h1>
          Quetz<span style={{ color: "var(--accent)" }}>al</span>
        </h1>
        {sent ? (
          <p className="muted">
            {t("If an account with that username or email exists and email is configured, a reset link is on its way.")}
          </p>
        ) : (
          <>
            <p className="muted">{t("Reset your password")}</p>
            <label>{t("Username or email")}</label>
            <input value={identifier} onChange={(e) => setIdentifier(e.target.value)} autoFocus />
            <button className="primary" style={{ marginTop: 16, width: "100%" }} disabled={busy || !identifier.trim()}>
              {busy ? "…" : t("Send reset link")}
            </button>
          </>
        )}
        <button type="button" className="link" style={{ marginTop: 8, width: "100%" }} onClick={onBack}>
          {t("Back to sign in")}
        </button>
      </form>
    </div>
  );
}
