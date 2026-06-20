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
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const u = setupNeeded
        ? await api.setup(username, password)
        : await api.login(username, password);
      onAuthed(u);
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
          {setupNeeded ? "Create the admin account" : "Sign in to continue"}
        </p>
        <label>Username</label>
        <input value={username} onChange={(e) => setUsername(e.target.value)} autoFocus />
        <label>Password</label>
        <input
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 16, width: "100%" }} disabled={busy}>
          {busy ? "…" : setupNeeded ? "Create admin" : "Sign in"}
        </button>
      </form>
    </div>
  );
}
