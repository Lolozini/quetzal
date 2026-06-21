import { useEffect, useState } from "react";
import { api, ApiError, AuditEntry, ExposeType, PowerAction, Server, ServerStats, User } from "../api";
import { Access } from "./Access";
import { Backups } from "./Backups";
import { Console } from "./Console";
import { Schedules } from "./Schedules";

function formatMem(bytes: number): string {
  if (bytes <= 0) return "0 MiB";
  const mib = bytes / (1024 * 1024);
  if (mib >= 1024) return `${(mib / 1024).toFixed(2)} GiB`;
  return `${mib.toFixed(0)} MiB`;
}

export function ServerDetail({ id, user, onBack }: { id: number; user: User; onBack: () => void }) {
  const [srv, setSrv] = useState<Server | null>(null);
  const [stats, setStats] = useState<ServerStats | null>(null);
  const [statsMsg, setStatsMsg] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState("");

  useEffect(() => {
    let active = true;
    const load = async () => {
      try {
        const s = await api.server(id);
        if (active) setSrv(s);
      } catch (e) {
        if (active) setError(String(e));
      }
      try {
        const st = await api.stats(id);
        if (active) {
          setStats(st);
          setStatsMsg("");
        }
      } catch (e) {
        if (active) {
          setStats(null);
          setStatsMsg(e instanceof ApiError ? e.message : String(e));
        }
      }
    };
    load();
    const t = setInterval(load, 4000);
    return () => {
      active = false;
      clearInterval(t);
    };
  }, [id]);

  async function changeExpose(type: ExposeType) {
    setError("");
    try {
      setSrv(await api.setExpose(id, { type }));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function suspend(want: boolean) {
    setError("");
    try {
      await (want ? api.suspend(id) : api.unsuspend(id));
      setSrv(await api.server(id));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  const canManage = !!srv && (user.isAdmin || srv.ownerId === user.id);

  const powerNotice: Record<PowerAction, string> = {
    start: "Start requested — the server is spinning up.",
    stop: "Stop requested — the server is shutting down gracefully.",
    restart: "Restart requested — the pod is being recreated; it will come back shortly.",
    kill: "Kill requested — forcing the pod to stop immediately.",
  };

  async function power(action: PowerAction) {
    setBusy(action);
    setError("");
    setNotice("");
    try {
      await api.power(id, action);
      setSrv(await api.server(id));
      setNotice(powerNotice[action]);
      window.setTimeout(() => setNotice(""), 6000);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy("");
    }
  }

  async function remove() {
    if (!window.confirm("Delete this server? Its pod, service and config will be removed.")) return;
    // Let the user decide the data lifecycle. For a PVC, "keep" retains the
    // underlying volume (Released PV); hostPath data is kept on the node anyway.
    const isPVC = srv?.storage?.type === "pvc";
    const keepData = isPVC
      ? window.confirm("Keep the data volume?\n\nOK = keep the data (retain the volume)\nCancel = destroy the data permanently")
      : false;
    try {
      await api.deleteServer(id, keepData);
      onBack();
    } catch (e) {
      setError(String(e));
    }
  }

  if (!srv) {
    return (
      <div className="card">
        <button onClick={onBack}>← Back</button>
        {error && <div className="error">{error}</div>}
        <p className="muted">Loading…</p>
      </div>
    );
  }

  return (
    <>
      <div className="card">
        <div className="row">
          <button onClick={onBack}>← Back</button>
          <div className="spacer" />
          <button className="danger" onClick={remove}>
            Delete
          </button>
        </div>
        <h2>
          {srv.displayName}{" "}
          <span className={`badge ${srv.status.phase}`}>{srv.status.phase}</span>
        </h2>
        <div className="kv">
          <span className="k">Desired state</span>
          <span>{srv.desiredState}</span>
        </div>
        <div className="kv">
          <span className="k">Image</span>
          <span>{srv.image}</span>
        </div>
        <div className="kv">
          <span className="k">Namespace</span>
          <span>{srv.namespace}</span>
        </div>
        {srv.status.address && (
          <div className="kv">
            <span className="k">Connect</span>
            <span>
              <code>{srv.status.address}</code>
            </span>
          </div>
        )}
        <div className="kv">
          <span className="k">Endpoints</span>
          <span>{(srv.status.endpoints || []).join(", ") || "—"}</span>
        </div>
        {srv.ports && srv.ports.length > 0 && (
          <div className="kv">
            <span className="k">Exposure</span>
            <span>
              <select
                value={srv.expose?.type || "ClusterIP"}
                onChange={(e) => changeExpose(e.target.value as ExposeType)}
              >
                <option value="ClusterIP">ClusterIP</option>
                <option value="NodePort">NodePort</option>
                <option value="LoadBalancer">LoadBalancer</option>
              </select>
            </span>
          </div>
        )}
        <div className="kv">
          <span className="k">CPU / Memory</span>
          <span>
            {stats
              ? `${stats.cpuMillicores}m${
                  stats.cpuLimit ? ` / ${stats.cpuLimit}` : ""
                }  ·  ${formatMem(stats.memoryBytes)}${
                  stats.memoryLimit ? ` / ${stats.memoryLimit}` : ""
                }`
              : statsMsg || "—"}
          </span>
        </div>
        {srv.status.message && (
          <div className="kv">
            <span className="k">Message</span>
            <span>{srv.status.message}</span>
          </div>
        )}
        <div className="row" style={{ marginTop: 12 }}>
          <button className="primary" disabled={busy !== ""} onClick={() => power("start")}>
            {busy === "start" ? "Starting…" : "Start"}
          </button>
          <button disabled={busy !== ""} onClick={() => power("stop")}>
            {busy === "stop" ? "Stopping…" : "Stop"}
          </button>
          <button disabled={busy !== ""} onClick={() => power("restart")}>
            {busy === "restart" ? "Restarting…" : "Restart"}
          </button>
          <button className="danger" disabled={busy !== ""} onClick={() => power("kill")}>
            {busy === "kill" ? "Killing…" : "Kill"}
          </button>
          {srv.hibernated && (
            <button className="primary" disabled={busy !== ""} onClick={() => power("start")}>Wake</button>
          )}
          {user.isAdmin && (
            <>
              <div className="spacer" />
              {srv.desiredState === "Suspended" ? (
                <button onClick={() => suspend(false)}>Unsuspend</button>
              ) : (
                <button className="danger" onClick={() => suspend(true)}>Suspend</button>
              )}
            </>
          )}
        </div>
        {canManage && srv.ports && srv.ports.length > 0 && (
          <div className="kv" style={{ marginTop: 12 }}>
            <span className="k">Hibernation</span>
            <span>
              <label className="row" style={{ width: "auto" }}>
                <input
                  type="checkbox"
                  style={{ width: "auto" }}
                  checked={!!srv.hibernation?.enabled}
                  onChange={(e) =>
                    api
                      .setHibernation(id, {
                        enabled: e.target.checked,
                        idleMinutes: srv.hibernation?.idleMinutes || 15,
                      })
                      .then(setSrv)
                      .catch((err) => setError(String(err)))
                  }
                />
                &nbsp;auto-sleep when idle after&nbsp;
              </label>
              <input
                type="number"
                min={1}
                style={{ width: 70 }}
                value={srv.hibernation?.idleMinutes || 15}
                onChange={(e) =>
                  api
                    .setHibernation(id, {
                      enabled: !!srv.hibernation?.enabled,
                      idleMinutes: Number(e.target.value),
                    })
                    .then(setSrv)
                    .catch((err) => setError(String(err)))
                }
              />
              &nbsp;min
            </span>
          </div>
        )}
        {notice && <div className="notice">{notice}</div>}
        {error && <div className="error">{error}</div>}
      </div>
      <Schedules id={id} />
      <Backups id={id} />
      {canManage && <Access id={id} />}
      <ServerAudit id={id} />
      <div className="card">
        <Console id={id} />
      </div>
    </>
  );
}

function ServerAudit({ id }: { id: number }) {
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  useEffect(() => {
    const load = () => api.serverAudit(id).then(setEntries).catch(() => {});
    load();
    const t = setInterval(load, 5000);
    return () => clearInterval(t);
  }, [id]);
  return (
    <div className="card">
      <h3>Activity</h3>
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
