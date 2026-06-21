import { FormEvent, useEffect, useState } from "react";
import { api, APIKey, ApiError, User } from "../api";

export function Account({ user }: { user: User }) {
  return (
    <>
      <ChangePassword />
      <APIKeys />
      <div className="card">
        <h3>Account</h3>
        <div className="kv"><span className="k">Username</span><span>{user.username}</span></div>
        <div className="kv"><span className="k">Role</span><span>{user.isAdmin ? "administrator" : "user"}</span></div>
      </div>
    </>
  );
}

function ChangePassword() {
  const [oldPassword, setOld] = useState("");
  const [newPassword, setNew] = useState("");
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");

  async function submit(e: FormEvent) {
    e.preventDefault();
    setMsg("");
    setError("");
    try {
      await api.changePassword(oldPassword, newPassword);
      setOld("");
      setNew("");
      setMsg("Password changed.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  return (
    <div className="card">
      <h2>Change password</h2>
      <form onSubmit={submit}>
        <label>Current password</label>
        <input type="password" autoComplete="current-password" value={oldPassword} onChange={(e) => setOld(e.target.value)} required />
        <label>New password</label>
        <input type="password" autoComplete="new-password" value={newPassword} onChange={(e) => setNew(e.target.value)} required />
        {msg && <div className="notice">{msg}</div>}
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={!oldPassword || !newPassword}>Update password</button>
      </form>
    </div>
  );
}

function APIKeys() {
  const [keys, setKeys] = useState<APIKey[]>([]);
  const [name, setName] = useState("");
  const [fresh, setFresh] = useState("");
  const [error, setError] = useState("");

  async function load() {
    try {
      setKeys(await api.apiKeys());
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, []);

  async function create(e: FormEvent) {
    e.preventDefault();
    setError("");
    try {
      const res = await api.createAPIKey(name);
      setFresh(res.token);
      setName("");
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  async function remove(k: APIKey) {
    if (!window.confirm(`Revoke API key "${k.name}"?`)) return;
    await api.deleteAPIKey(k.id).catch((e) => setError(String(e)));
    await load();
  }

  return (
    <div className="card">
      <h2>API keys</h2>
      <p className="muted">
        Use as a bearer token: <code>Authorization: Bearer &lt;token&gt;</code>. A key inherits your permissions.
      </p>
      {fresh && (
        <div className="notice">
          New token (shown once — copy it now): <code style={{ wordBreak: "break-all" }}>{fresh}</code>
        </div>
      )}
      {keys.length === 0 ? (
        <p className="muted">No API keys.</p>
      ) : (
        <table>
          <thead><tr><th>Name</th><th>Prefix</th><th>Last used</th><th></th></tr></thead>
          <tbody>
            {keys.map((k) => (
              <tr key={k.id}>
                <td>{k.name}</td>
                <td><code>{k.prefix}…</code></td>
                <td>{k.lastUsedAt ? new Date(k.lastUsedAt).toLocaleString() : "never"}</td>
                <td><button className="danger" onClick={() => remove(k)}>Revoke</button></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <form onSubmit={create} className="row" style={{ marginTop: 12 }}>
        <input value={name} placeholder="key name (e.g. ci)" onChange={(e) => setName(e.target.value)} required />
        <button className="primary" disabled={!name}>Create key</button>
      </form>
      {error && <div className="error">{error}</div>}
    </div>
  );
}
