import { useEffect, useState } from "react";
import { api, ApiError, ServerDatabase } from "../api";

// Databases lists and provisions a server's databases (a schema + scoped user on
// a registered host). Credentials are shown on demand.
export function Databases({ serverId }: { serverId: number }) {
  const [dbs, setDbs] = useState<ServerDatabase[]>([]);
  const [hosts, setHosts] = useState<{ id: number; name: string; kind: string; full: boolean }[]>([]);
  const [hostId, setHostId] = useState<number>(0);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  // Revealed credentials per database id (password fetched on demand).
  const [reveal, setReveal] = useState<Record<number, ServerDatabase>>({});

  async function load() {
    try {
      setDbs(await api.serverDatabases(serverId));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
    api.serverDatabaseHosts(serverId).then((hs) => {
      setHosts(hs);
      const first = hs.find((h) => !h.full);
      if (first) setHostId(first.id);
    }).catch(() => {});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [serverId]);

  async function create() {
    if (!hostId) return;
    setBusy(true);
    setError("");
    try {
      const d = await api.createServerDatabase(serverId, hostId);
      setReveal((m) => ({ ...m, [d.id]: d })); // show credentials right away
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  async function show(d: ServerDatabase) {
    try {
      const full = await api.getServerDatabase(serverId, d.id);
      setReveal((m) => ({ ...m, [d.id]: full }));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function rotate(d: ServerDatabase) {
    if (!window.confirm(`Rotate the password for "${d.databaseName}"? Anything using the old password will stop working.`)) return;
    try {
      const rotated = await api.rotateServerDatabase(serverId, d.id);
      setReveal((m) => ({ ...m, [d.id]: rotated }));
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function remove(d: ServerDatabase) {
    if (!window.confirm(`Delete database "${d.databaseName}"? This drops the database and its data.`)) return;
    try {
      await api.deleteServerDatabase(serverId, d.id);
      setReveal((m) => {
        const c = { ...m };
        delete c[d.id];
        return c;
      });
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <div className="card">
      <h2>Databases</h2>
      {dbs.length === 0 ? (
        <p className="muted">No databases yet.</p>
      ) : (
        dbs.map((d) => {
          const r = reveal[d.id];
          return (
            <div key={d.id} className="card" style={{ background: "var(--panel-2)", marginBottom: 8 }}>
              <div className="kv"><span className="k">Database</span><span><code>{d.databaseName}</code></span></div>
              <div className="kv"><span className="k">Username</span><span><code>{d.username}</code></span></div>
              <div className="kv"><span className="k">Endpoint</span><span><code>{d.host}:{d.port}</code>{d.hostName ? ` (${d.hostName})` : ""}</span></div>
              {r?.password && <div className="kv"><span className="k">Password</span><span><code>{r.password}</code></span></div>}
              <div className="row" style={{ marginTop: 8 }}>
                {!r?.password && <button onClick={() => show(d)}>Show password</button>}
                <button onClick={() => rotate(d)}>Rotate password</button>
                <button className="danger" onClick={() => remove(d)}>Delete</button>
              </div>
            </div>
          );
        })
      )}

      <div className="row" style={{ marginTop: 12 }}>
        {hosts.length === 0 ? (
          <span className="muted">No database hosts are configured. Ask an admin to add one.</span>
        ) : (
          <>
            <select value={hostId} onChange={(e) => setHostId(Number(e.target.value))}>
              {hosts.map((h) => (
                <option key={h.id} value={h.id} disabled={h.full}>
                  {h.name} ({h.kind}){h.full ? " — full" : ""}
                </option>
              ))}
            </select>
            <button className="primary" onClick={create} disabled={busy || !hostId}>
              {busy ? "Creating…" : "Create database"}
            </button>
          </>
        )}
      </div>
      {error && <div className="error">{error}</div>}
    </div>
  );
}
