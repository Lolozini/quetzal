import { useEffect, useState } from "react";
import { api, ApiError, ExposeType, PowerAction, Server, ServerStats } from "../api";
import { Console } from "./Console";

function formatMem(bytes: number): string {
  if (bytes <= 0) return "0 MiB";
  const mib = bytes / (1024 * 1024);
  if (mib >= 1024) return `${(mib / 1024).toFixed(2)} GiB`;
  return `${mib.toFixed(0)} MiB`;
}

export function ServerDetail({ id, onBack }: { id: number; onBack: () => void }) {
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
    if (!window.confirm("Delete this server? Its namespace and data will be removed.")) return;
    try {
      await api.deleteServer(id);
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
        </div>
        {notice && <div className="notice">{notice}</div>}
        {error && <div className="error">{error}</div>}
      </div>
      <div className="card">
        <Console id={id} />
      </div>
    </>
  );
}
