import { useId, FormEvent, useEffect, useState } from "react";
import { ALL_PERMISSIONS, api, ApiError, InviteInfo, User } from "../api";
import { useT } from "../i18n";
import { Auth } from "./Auth";
import { Lockup } from "./Brand";
import { permissionHelp } from "./Access";

// Invite is shown when the app loads with a #invite=<token> link from an
// invitation email. It says what the link offers, then accepts it from the
// account signed in, one the reader signs in to, or one they create from it.
export function Invite({
  token,
  user,
  onAuthed,
  onDone,
}: {
  token: string;
  user: User | null;
  onAuthed: (u: User) => void;
  onDone: (serverId?: number) => void;
}) {
  const fieldId = useId();
  const { t } = useT();
  const [info, setInfo] = useState<InviteInfo | null>(null);
  const [invalid, setInvalid] = useState("");
  const [mode, setMode] = useState<"choose" | "signin" | "signup">("choose");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api.inspectInvite(token)
      .then(setInfo)
      .catch((e) => setInvalid(e instanceof ApiError ? e.message : String(e)));
  }, [token]);

  async function accept() {
    setError("");
    setBusy(true);
    try {
      const res = await api.acceptInvite(token);
      onDone(res.serverId);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  async function register(e: FormEvent) {
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
      const res = await api.registerFromInvite(token, username.trim(), password);
      // /api/me also says whether the panel wants a second factor first.
      onAuthed(await api.me().catch(() => res.user));
      onDone(res.serverId);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  // Signing in is the panel's own screen; once it is done, the user comes back
  // here, signed in, to accept.
  if (mode === "signin" && !user) {
    return <Auth setupNeeded={false} onAuthed={onAuthed} />;
  }

  return (
    <div className="center auth">
      <Lockup stacked />
      <div className="card" style={{ width: 440, maxWidth: "100%" }}>
        {invalid ? (
          <>
            <h2>{t("This invitation is no longer valid")}</h2>
            <p className="muted">
              {t("It was used, withdrawn, or is more than 7 days old. Ask whoever sent it for a new one.")}
            </p>
            <button type="button" className="primary" style={{ marginTop: 16, width: "100%" }} onClick={() => onDone()}>
              {t("Go to the panel")}
            </button>
          </>
        ) : !info ? (
          <p className="muted">{t("Loading…")}</p>
        ) : (
          <>
            <h2>{t("You are invited to {server}", { server: info.server })}</h2>
            <p className="muted">
              {t("{name} invited {email}, with access to:", { name: info.invitedBy || t("Someone"), email: info.email })}
            </p>
            <ul className="perm-summary">
              {info.permissions.map((p) => (
                <li key={p}>
                  <b>{p}</b>{" "}
                  {ALL_PERMISSIONS.includes(p as (typeof ALL_PERMISSIONS)[number]) && (
                    <span className="muted">{permissionHelp(t, p as (typeof ALL_PERMISSIONS)[number])}</span>
                  )}
                </li>
              ))}
            </ul>
            <p className="muted">
              {t("The link is good until {date}.", { date: new Date(info.expiresAt).toLocaleString() })}
            </p>

            {user ? (
              <>
                <button className="primary" style={{ marginTop: 12, width: "100%" }} onClick={accept} disabled={busy}>
                  {busy ? "…" : t("Accept as {username}", { username: user.username })}
                </button>
                <button
                  type="button"
                  className="link"
                  style={{ marginTop: 8, width: "100%" }}
                  onClick={async () => {
                    await api.logout().catch(() => {});
                    window.location.reload();
                  }}
                >
                  {t("Not you? Sign out")}
                </button>
              </>
            ) : mode === "signup" ? (
              <form onSubmit={register}>
                <label htmlFor={`${fieldId}-username`}>{t("Username")}</label>
                <input id={`${fieldId}-username`} value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" autoFocus required />
                <label htmlFor={`${fieldId}-password`}>{t("Password")}</label>
                <input id={`${fieldId}-password`} type="password" autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} required />
                <label htmlFor={`${fieldId}-confirm-password`}>{t("Confirm password")}</label>
                <input id={`${fieldId}-confirm-password`} type="password" autoComplete="new-password" value={confirm} onChange={(e) => setConfirm(e.target.value)} required />
                <p className="muted" style={{ marginTop: 8 }}>
                  {t("Your account reaches the servers it is invited to. Its email is {email}.", { email: info.email })}
                </p>
                {error && <div className="error">{error}</div>}
                <button className="primary" style={{ marginTop: 12, width: "100%" }} disabled={busy}>
                  {busy ? "…" : t("Create my account and accept")}
                </button>
                <button type="button" className="link" style={{ marginTop: 8, width: "100%" }} onClick={() => setMode("choose")}>
                  {t("Back")}
                </button>
              </form>
            ) : (
              <>
                <button className="primary" style={{ marginTop: 12, width: "100%" }} onClick={() => setMode("signin")}>
                  {t("Sign in to accept")}
                </button>
                {info.signup ? (
                  <button style={{ marginTop: 8, width: "100%" }} onClick={() => setMode("signup")}>
                    {t("Create an account")}
                  </button>
                ) : (
                  <p className="muted" style={{ marginTop: 8 }}>
                    {t("This panel does not create accounts from invitations: sign in to the account you have here.")}
                  </p>
                )}
              </>
            )}
            {user && error && <div className="error">{error}</div>}
            <button type="button" className="link" style={{ marginTop: 8, width: "100%" }} onClick={() => onDone()}>
              {t("Not now")}
            </button>
          </>
        )}
      </div>
    </div>
  );
}
