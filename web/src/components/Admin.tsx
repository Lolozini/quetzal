import { FormEvent, useEffect, useState } from "react";
import { api, ApiError, AuditEntry, User } from "../api";

export function Admin() {
  return (
    <>
      <Users />
      <GlobalAudit />
    </>
  );
}

function Users() {
  const [users, setUsers] = useState<User[]>([]);
  const [error, setError] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [isAdmin, setIsAdmin] = useState(false);
  const [maxServers, setMaxServers] = useState(0);
  const [maxMemoryMB, setMaxMemoryMB] = useState(0);
  const [busy, setBusy] = useState(false);

  async function load() {
    try {
      setUsers(await api.users());
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, []);

  async function add(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.createUser({ username, password, isAdmin, maxServers, maxMemoryMB });
      setUsername("");
      setPassword("");
      setIsAdmin(false);
      setMaxServers(0);
      setMaxMemoryMB(0);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function toggleAdmin(u: User) {
    setError("");
    try {
      await api.updateUser(u.id, {
        isAdmin: !u.isAdmin,
        maxServers: u.maxServers ?? 0,
        maxMemoryMB: u.maxMemoryMB ?? 0,
        maxCpuMilli: u.maxCpuMilli ?? 0,
      });
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function remove(u: User) {
    if (!window.confirm(`Delete user "${u.username}"? Their servers are NOT deleted.`)) return;
    try {
      await api.deleteUser(u.id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <div className="card">
      <h2>Users</h2>
      <table>
        <thead>
          <tr><th>User</th><th>Role</th><th>Quota (servers / mem MB)</th><th></th></tr>
        </thead>
        <tbody>
          {users.map((u) => (
            <tr key={u.id}>
              <td>{u.username}</td>
              <td>{u.isAdmin ? "admin" : "user"}</td>
              <td>{(u.maxServers || "∞") + " / " + (u.maxMemoryMB || "∞")}</td>
              <td style={{ whiteSpace: "nowrap" }}>
                <button onClick={() => toggleAdmin(u)}>{u.isAdmin ? "Demote" : "Make admin"}</button>{" "}
                <button className="danger" onClick={() => remove(u)}>Delete</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      <form onSubmit={add} style={{ marginTop: 12 }}>
        <h3>New user</h3>
        <div className="grid2">
          <div><label>Username</label><input value={username} onChange={(e) => setUsername(e.target.value)} required /></div>
          <div><label>Password</label><input type="password" autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} required /></div>
        </div>
        <div className="grid2">
          <div><label>Max servers (0 = ∞)</label><input type="number" min={0} value={maxServers} onChange={(e) => setMaxServers(Number(e.target.value))} /></div>
          <div><label>Max memory MB (0 = ∞)</label><input type="number" min={0} value={maxMemoryMB} onChange={(e) => setMaxMemoryMB(Number(e.target.value))} /></div>
        </div>
        <label className="row"><input type="checkbox" style={{ width: "auto" }} checked={isAdmin} onChange={(e) => setIsAdmin(e.target.checked)} />&nbsp;Administrator</label>
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={busy || !username || !password}>
          {busy ? "Creating…" : "Create user"}
        </button>
      </form>
    </div>
  );
}

function GlobalAudit() {
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  useEffect(() => {
    api.globalAudit().then(setEntries).catch(() => {});
  }, []);
  return (
    <div className="card">
      <h2>Activity log</h2>
      {entries.length === 0 ? (
        <p className="muted">No activity yet.</p>
      ) : (
        <table>
          <thead><tr><th>When</th><th>User</th><th>Action</th><th>Detail</th></tr></thead>
          <tbody>
            {entries.map((e) => (
              <tr key={e.id}>
                <td>{new Date(e.createdAt).toLocaleString()}</td>
                <td>{e.username}</td>
                <td><code>{e.action}</code></td>
                <td>{e.detail}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
