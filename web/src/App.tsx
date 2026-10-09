import { useCallback, useEffect, useState } from "react";
import { api, User } from "./api";
import { useT } from "./i18n";
import { Auth } from "./components/Auth";
import { ResetPassword } from "./components/ResetPassword";
import { ConfirmEmail } from "./components/ConfirmEmail";
import { Invite } from "./components/Invite";
import { Dashboard } from "./components/Dashboard";
import { TwoFactor } from "./components/Account";

export function App() {
  const { t } = useT();
  const [loading, setLoading] = useState(true);
  const [setupNeeded, setSetupNeeded] = useState(false);
  const [setupCodeRequired, setSetupCodeRequired] = useState(false);
  const [user, setUser] = useState<User | null>(null);
  // A reset link (emailed as <panel>/#reset=<token>) lands here. The token is in
  // the URL fragment so it's never sent to the server (or upstream proxy logs).
  const [resetToken, setResetToken] = useState<string | null>(
    () => new URLSearchParams(window.location.hash.replace(/^#/, "")).get("reset"),
  );
  // A confirmation link (<panel>/#confirm-email=<token>), in the fragment for
  // the same reason.
  const [confirmToken, setConfirmToken] = useState<string | null>(
    () => new URLSearchParams(window.location.hash.replace(/^#/, "")).get("confirm-email"),
  );
  // An invitation link (<panel>/#invite=<token>), in the fragment for the same
  // reason.
  const [inviteToken, setInviteToken] = useState<string | null>(
    () => new URLSearchParams(window.location.hash.replace(/^#/, "")).get("invite"),
  );
  const invite = inviteToken && (
    <Invite
      token={inviteToken}
      user={user}
      onAuthed={(u) => {
        setUser(u);
        setSetupNeeded(false);
      }}
      onDone={(serverId) => {
        // Drop the token from the URL; open the server when there is one.
        window.history.replaceState(null, "", window.location.pathname + (serverId ? `#/servers/${serverId}` : ""));
        setInviteToken(null);
      }}
    />
  );

  useEffect(() => {
    (async () => {
      try {
        const s = await api.setupStatus();
        setSetupNeeded(s.needed);
        setSetupCodeRequired(!!s.codeRequired);
        if (!s.needed) {
          try {
            setUser(await api.me());
          } catch {
            setUser(null);
          }
        }
      } finally {
        setLoading(false);
      }
    })();
  }, []);

  useEffect(() => {
    const onUnauthorized = () => setUser(null);
    window.addEventListener("quetzal:unauthorized", onUnauthorized);
    return () => window.removeEventListener("quetzal:unauthorized", onUnauthorized);
  }, []);

  const refreshUser = useCallback(async () => {
    setUser(await api.me());
  }, []);

  if (resetToken) {
    return (
      <ResetPassword
        token={resetToken}
        onDone={() => {
          // Drop the token from the URL and return to the login screen.
          window.history.replaceState(null, "", window.location.pathname);
          setResetToken(null);
        }}
      />
    );
  }

  if (confirmToken) {
    return (
      <ConfirmEmail
        token={confirmToken}
        onDone={() => {
          window.history.replaceState(null, "", window.location.pathname);
          setConfirmToken(null);
          // The address on the account changed: read it again.
          api.me().then(setUser).catch(() => {});
        }}
      />
    );
  }

  if (loading) return <div className="center muted">{t("Loading…")}</div>;

  if (!user && invite && !setupNeeded) return invite;

  if (!user) {
    return (
      <Auth
        setupNeeded={setupNeeded}
        setupCodeRequired={setupCodeRequired}
        onAuthed={(u) => {
          setUser(u);
          setSetupNeeded(false);
        }}
      />
    );
  }

  // The panel requires a second factor and this account has none. Its session is
  // valid but reaches only enrolment, so showing the rest of the app would be a
  // wall of 403s; show the one thing that can be done instead.
  if (user.twoFactorRequired) {
    return (
      <div className="center">
        <div className="card" style={{ maxWidth: 520 }}>
          <h2>{t("Two-factor authentication required")}</h2>
          <p className="muted">
            {t("This panel requires a second factor. Set one up to carry on — nothing else is available until you do.")}
          </p>
          <TwoFactor
            initialEnabled={false}
            username={user.username}
            onChanged={refreshUser}
          />
          <button
            style={{ marginTop: 12 }}
            onClick={async () => {
              await api.logout();
              setUser(null);
            }}
          >
            {t("Logout")}
          </button>
        </div>
      </div>
    );
  }

  // After the second factor, not before: until then the session reaches
  // nothing, accepting included.
  if (invite) return invite;

  return (
    <Dashboard
      user={user}
      onUserRefresh={refreshUser}
      onLogout={async () => {
        await api.logout();
        setUser(null);
      }}
    />
  );
}
