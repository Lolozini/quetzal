import { useEffect, useState } from "react";
import { api, User } from "./api";
import { useT } from "./i18n";
import { Auth } from "./components/Auth";
import { ResetPassword } from "./components/ResetPassword";
import { Dashboard } from "./components/Dashboard";
import { TwoFactor } from "./components/Account";

export function App() {
  const { t } = useT();
  const [loading, setLoading] = useState(true);
  const [setupNeeded, setSetupNeeded] = useState(false);
  const [user, setUser] = useState<User | null>(null);
  // A reset link (emailed as <panel>/#reset=<token>) lands here. The token is in
  // the URL fragment so it's never sent to the server (or upstream proxy logs).
  const [resetToken, setResetToken] = useState<string | null>(
    () => new URLSearchParams(window.location.hash.replace(/^#/, "")).get("reset"),
  );

  useEffect(() => {
    (async () => {
      try {
        const s = await api.setupStatus();
        setSetupNeeded(s.needed);
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

  if (loading) return <div className="center muted">{t("Loading…")}</div>;

  if (!user) {
    return (
      <Auth
        setupNeeded={setupNeeded}
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
            onEnabled={() => api.me().then(setUser).catch(() => {})}
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

  return (
    <Dashboard
      user={user}
      onLogout={async () => {
        await api.logout();
        setUser(null);
      }}
    />
  );
}
