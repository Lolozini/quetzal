import { useId, FormEvent, useState } from "react";
import { api, ApiError, User } from "../api";
import { LangSwitcher, useT } from "../i18n";
import { Lockup } from "./Brand";

export function Auth({
  setupNeeded,
  setupCodeRequired = false,
  onAuthed,
}: {
  setupNeeded: boolean;
  // The first account is a superadmin: its setup asks for the code the
  // panel prints in its log, so that whoever reaches the page first cannot
  // claim the install.
  setupCodeRequired?: boolean;
  onAuthed: (u: User) => void;
}) {
  const fieldId = useId();
  const { t } = useT();
  const [setupCode, setSetupCode] = useState("");
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
        onAuthed(await api.setup(username, password, email.trim() || undefined, setupCode.trim() || undefined));
        return;
      }
      const res = await api.login(username, password, twoFactor ? code : undefined);
      if (!("id" in res) && res.twoFactorRequired) {
        setTwoFactor(true); // ask for the code, keep username/password
      } else {
        // Login returns the account; /me also includes session policy flags.
        onAuthed(await api.me());
      }
    } catch (err) {
      if (err instanceof ApiError && err.status === 429) {
        setError(t("Too many sign-in attempts. Try again in a few minutes."));
      } else {
        setError(err instanceof ApiError ? err.message : String(err));
      }
    } finally {
      setBusy(false);
    }
  }

  if (forgot) {
    return <Forgot onBack={() => setForgot(false)} />;
  }

  return (
    <div className="center auth">
      <Lockup stacked />
      <form className="card" style={{ width: 360 }} onSubmit={submit}>
        <p className="muted">
          {setupNeeded
            ? t("Create the admin account")
            : twoFactor
              ? t("Enter your authentication code")
              : t("Sign in to continue")}
        </p>
        {setupNeeded && setupCodeRequired && (
          <>
            <label htmlFor={`${fieldId}-setup-code`}>{t("Setup code")}</label>
            <input aria-describedby={`${fieldId}-setup-help`} id={`${fieldId}-setup-code`}
              value={setupCode}
              onChange={(e) => setSetupCode(e.target.value)}
              autoComplete="off"
              autoFocus
              placeholder="XXXX-XXXX-XXXX"
            />
            <p id={`${fieldId}-setup-help`} className="muted" style={{ fontSize: 12 }}>
              {t("The panel prints it in its log:")} <code>kubectl -n quetzal logs deploy/quetzal -c apiserver</code>
            </p>
          </>
        )}
        {!twoFactor && (
          <>
            <label htmlFor={`${fieldId}-username`}>{t("Username")}</label>
            <input id={`${fieldId}-username`} value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" autoFocus={!(setupNeeded && setupCodeRequired)} />
            <label htmlFor={`${fieldId}-password`}>{t("Password")}</label>
            {/* Says to a password manager which password this is: the one to
                fill in, or a new one to save on the setup screen. */}
            <input id={`${fieldId}-password`}
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete={setupNeeded ? "new-password" : "current-password"}
            />
            {setupNeeded && (
              <>
                <label htmlFor={`${fieldId}-email`}>{t("Email (optional, for password reset)")}</label>
                <input id={`${fieldId}-email`}
                  type="email"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  autoComplete="email"
                  placeholder="you@example.com"
                />
              </>
            )}
          </>
        )}
        {twoFactor && (
          <>
            <label htmlFor={`${fieldId}-authentication-code`}>{t("Authentication code")}</label>
            <input aria-describedby={`${fieldId}-code-help`} id={`${fieldId}-authentication-code`}
              value={code}
              onChange={(e) => setCode(e.target.value)}
              autoFocus
              autoComplete="one-time-code"
              inputMode="text"
              placeholder={t("6-digit code or recovery code")}
            />
            <p id={`${fieldId}-code-help`} className="muted">{t("From your authenticator app, or a recovery code.")}</p>
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
  const fieldId = useId();
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
    <div className="center auth">
      <Lockup stacked />
      <form className="card" style={{ width: 360 }} onSubmit={submit}>
        {sent ? (
          <p className="muted">
            {t("If an account with that username or email exists and email is configured, a reset link is on its way.")}
          </p>
        ) : (
          <>
            <p className="muted">{t("Reset your password")}</p>
            <label htmlFor={`${fieldId}-identifier`}>{t("Username or email")}</label>
            <input id={`${fieldId}-identifier`} value={identifier} onChange={(e) => setIdentifier(e.target.value)} autoComplete="username" autoFocus />
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
