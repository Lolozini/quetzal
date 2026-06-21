import { FormEvent, useEffect, useState } from "react";
import { ALL_PERMISSIONS, api, ApiError, ServerAccess } from "../api";

export function Access({ id }: { id: number }) {
  const [list, setList] = useState<ServerAccess[]>([]);
  const [username, setUsername] = useState("");
  const [perms, setPerms] = useState<string[]>(["view"]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function load() {
    try {
      setList(await api.access(id));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, [id]);

  function toggle(p: string) {
    setPerms((cur) => (cur.includes(p) ? cur.filter((x) => x !== p) : [...cur, p]));
  }

  async function grant(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.grantAccess(id, username, perms);
      setUsername("");
      setPerms(["view"]);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function revoke(a: ServerAccess) {
    if (!window.confirm(`Revoke ${a.username}'s access?`)) return;
    await api.revokeAccess(id, a.userId).catch((e) => setError(String(e)));
    await load();
  }

  return (
    <div className="card">
      <h3>Subusers</h3>
      {list.length === 0 ? (
        <p className="muted">No subusers. Grant another account scoped access below.</p>
      ) : (
        <table>
          <thead><tr><th>User</th><th>Permissions</th><th></th></tr></thead>
          <tbody>
            {list.map((a) => (
              <tr key={a.id}>
                <td>{a.username}</td>
                <td>{a.permissions.join(", ")}</td>
                <td><button className="danger" onClick={() => revoke(a)}>Revoke</button></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <form onSubmit={grant} style={{ marginTop: 12 }}>
        <label>Username</label>
        <input value={username} onChange={(e) => setUsername(e.target.value)} placeholder="existing account" required />
        <label>Permissions</label>
        <div className="row" style={{ flexWrap: "wrap", gap: 12 }}>
          {ALL_PERMISSIONS.map((p) => (
            <label key={p} className="row" style={{ width: "auto" }}>
              <input type="checkbox" style={{ width: "auto" }} checked={perms.includes(p)} onChange={() => toggle(p)} />
              &nbsp;{p}
            </label>
          ))}
        </div>
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={busy || !username || perms.length === 0}>
          {busy ? "Granting…" : "Grant access"}
        </button>
      </form>
    </div>
  );
}
