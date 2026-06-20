import { useEffect, useState } from "react";
import { api, User } from "./api";
import { Auth } from "./components/Auth";
import { Dashboard } from "./components/Dashboard";

export function App() {
  const [loading, setLoading] = useState(true);
  const [setupNeeded, setSetupNeeded] = useState(false);
  const [user, setUser] = useState<User | null>(null);

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

  if (loading) return <div className="center muted">Loading…</div>;

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
