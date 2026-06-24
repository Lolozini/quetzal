import { useEffect, useState } from "react";
import { api, User } from "./api";
import { useT } from "./i18n";
import { Auth } from "./components/Auth";
import { ResetPassword } from "./components/ResetPassword";
import { Dashboard } from "./components/Dashboard";

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
