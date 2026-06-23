import { useEffect, useState } from "react";
import { api, ApiError, AuditEntry, Cluster, ExposeType, PowerAction, Server, ServerStats, User } from "../api";
import { Access } from "./Access";
import { Backups } from "./Backups";
import { Console } from "./Console";
import { Databases } from "./Databases";
import { Files } from "./Files";
import { Notifications } from "./Notifications";
import { Schedules } from "./Schedules";
import { ServerSettings } from "./ServerSettings";

function formatMem(bytes: number): string {
  if (bytes <= 0) return "0 MiB";
  const mib = bytes / (1024 * 1024);
  if (mib >= 1024) return `${(mib / 1024).toFixed(2)} GiB`;
  return `${mib.toFixed(0)} MiB`;
}

function formatRate(bps: number): string {
  if (!isFinite(bps) || bps <= 0) return "0 B/s";
  const units = ["B", "KiB", "MiB", "GiB"];
  let v = bps;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}/s`;
}

// A rolling sample of a server's live resource usage, kept client-side to draw
// the time-series charts (CPU/RAM/network); rx/tx are cumulative counters.
interface Sample {
  t: number;
  cpu: number;
  mem: number;
  rx?: number;
  tx?: number;
}

// Roughly 4 minutes of history at the 4s poll interval.
const MAX_SAMPLES = 60;

// Chart is a dependency-free SVG sparkline (filled area + line) for one series.
function Chart({ label, value, points, color }: { label: string; value: string; points: number[]; color: string }) {
  const w = 240;
  const h = 48;
  const max = Math.max(1, ...points);
  const path = points
    .map((p, i) => {
      const x = points.length <= 1 ? 0 : (i / (points.length - 1)) * w;
      const y = h - (Math.max(0, p) / max) * (h - 2) - 1;
      return `${i === 0 ? "M" : "L"}${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");
  return (
    <div className="chart">
      <div className="chart-head">
        <span className="muted">{label}</span>
        <strong>{value}</strong>
      </div>
      <svg viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" className="spark">
        {points.length > 1 && <path d={`${path} L${w},${h} L0,${h} Z`} fill={color} fillOpacity={0.15} />}
        {points.length > 1 && <path d={path} fill="none" stroke={color} strokeWidth={1.5} vectorEffect="non-scaling-stroke" />}
      </svg>
    </div>
  );
}

function DiskBar({ used, total }: { used: number; total: number }) {
  const pct = total > 0 ? Math.min(100, (used / total) * 100) : 0;
  return (
    <div className="chart">
      <div className="chart-head">
        <span className="muted">Disk</span>
        <strong>{`${formatMem(used)} / ${formatMem(total)} (${pct.toFixed(0)}%)`}</strong>
      </div>
      <div className="diskbar-track">
        <div className="diskbar-fill" style={{ width: `${pct}%`, background: pct > 90 ? "var(--danger)" : "var(--accent-2)" }} />
      </div>
    </div>
  );
}

function StatsPanel({ stats, history, statsMsg }: { stats: ServerStats | null; history: Sample[]; statsMsg: string }) {
  if (!stats) {
    return (
      <div className="kv">
        <span className="k">Resources</span>
        <span>{statsMsg || "—"}</span>
      </div>
    );
  }
  // Derive network rates from successive cumulative counters. Only rate a pair
  // when both samples carry counters (a gap from an intermittent exec failure
  // would otherwise read as a huge spike on recovery); a counter reset from a
  // pod restart shows as a negative delta, clamped to 0.
  const rxRate: number[] = [];
  const txRate: number[] = [];
  for (let i = 1; i < history.length; i++) {
    const a = history[i - 1];
    const b = history[i];
    if (a.rx === undefined || b.rx === undefined || a.tx === undefined || b.tx === undefined) continue;
    const dt = (b.t - a.t) / 1000;
    if (dt <= 0) continue;
    const drx = b.rx - a.rx;
    const dtx = b.tx - a.tx;
    rxRate.push(drx >= 0 ? drx / dt : 0);
    txRate.push(dtx >= 0 ? dtx / dt : 0);
  }
  const hasNet = history.some((s) => s.rx !== undefined);
  return (
    <div className="charts">
      <Chart
        label="CPU"
        value={`${stats.cpuMillicores}m${stats.cpuLimit ? ` / ${stats.cpuLimit}` : ""}`}
        points={history.map((s) => s.cpu)}
        color="var(--accent)"
      />
      <Chart
        label="Memory"
        value={`${formatMem(stats.memoryBytes)}${stats.memoryLimit ? ` / ${stats.memoryLimit}` : ""}`}
        points={history.map((s) => s.mem)}
        color="var(--accent-2)"
      />
      {hasNet && <Chart label="Net in" value={formatRate(rxRate[rxRate.length - 1] ?? 0)} points={rxRate} color="#3fb950" />}
      {hasNet && <Chart label="Net out" value={formatRate(txRate[txRate.length - 1] ?? 0)} points={txRate} color="#d29922" />}
      {stats.diskTotalBytes ? <DiskBar used={stats.diskUsedBytes ?? 0} total={stats.diskTotalBytes} /> : null}
    </div>
  );
}

export function ServerDetail({ id, user, onBack }: { id: number; user: User; onBack: () => void }) {
  const [srv, setSrv] = useState<Server | null>(null);
  const [clusters, setClusters] = useState<Cluster[]>([]);
  const [stats, setStats] = useState<ServerStats | null>(null);
  const [history, setHistory] = useState<Sample[]>([]);
  const [statsMsg, setStatsMsg] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState("");

  useEffect(() => {
    let active = true;
    setHistory([]); // fresh buffer per server
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
          setHistory((h) =>
            [...h, { t: Date.now(), cpu: st.cpuMillicores, mem: st.memoryBytes, rx: st.rxBytes, tx: st.txBytes }].slice(-MAX_SAMPLES),
          );
        }
      } catch (e) {
        if (active) {
          setStats(null);
          setStatsMsg(e instanceof ApiError ? e.message : String(e));
        }
      }
    };
    load();
    api.clusters().then((cs) => active && setClusters(cs)).catch(() => {});
    const t = setInterval(load, 4000);
    return () => {
      active = false;
      clearInterval(t);
    };
  }, [id]);

  const clusterName = (cid?: number) => clusters.find((c) => c.id === cid)?.name;

  // saveHib patches the hibernation policy while preserving the unspecified
  // fields (so toggling one control never silently clears the others).
  function saveHib(patch: Partial<NonNullable<Server["hibernation"]>>) {
    const cur = srv?.hibernation;
    api
      .setHibernation(id, {
        enabled: cur?.enabled ?? false,
        idleMinutes: cur?.idleMinutes || 15,
        wakeOnConnect: cur?.wakeOnConnect ?? false,
        proxy: cur?.proxy ?? false,
        ...patch,
      })
      .then(setSrv)
      .catch((err) => setError(String(err)));
  }

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
  const hasPorts = !!srv?.ports && srv.ports.length > 0;
  // TCP-only servers can use the lightweight wake-on-connect; UDP needs the proxy.
  const tcpOnly = hasPorts && srv!.ports!.every((p) => p.protocol.toUpperCase() !== "UDP");

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
        {clusterName(srv.clusterId) && (
          <div className="kv">
            <span className="k">Cluster</span>
            <span>{clusterName(srv.clusterId)}</span>
          </div>
        )}
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
        <StatsPanel stats={stats} history={history} statsMsg={statsMsg} />
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
        {canManage && hasPorts && (
          <div className="kv" style={{ marginTop: 12 }}>
            <span className="k">Hibernation</span>
            <span>
              <label className="row" style={{ width: "auto" }}>
                <input
                  type="checkbox"
                  style={{ width: "auto" }}
                  checked={!!srv.hibernation?.enabled}
                  onChange={(e) => saveHib({ enabled: e.target.checked })}
                />
                &nbsp;auto-sleep when idle after&nbsp;
              </label>
              <input
                type="number"
                min={1}
                style={{ width: 70 }}
                value={srv.hibernation?.idleMinutes || 15}
                onChange={(e) => saveHib({ idleMinutes: Number(e.target.value) })}
              />
              &nbsp;min
              {srv.hibernation?.enabled && (
                <>
                  {tcpOnly && (
                    <label className="row" style={{ width: "auto", marginTop: 4 }}>
                      <input
                        type="checkbox"
                        style={{ width: "auto" }}
                        checked={!!srv.hibernation?.wakeOnConnect && !srv.hibernation?.proxy}
                        disabled={!!srv.hibernation?.proxy}
                        onChange={(e) => saveHib({ wakeOnConnect: e.target.checked })}
                      />
                      &nbsp;wake when a player connects (TCP)
                    </label>
                  )}
                  <label className="row" style={{ width: "auto", marginTop: 4 }}>
                    <input
                      type="checkbox"
                      style={{ width: "auto" }}
                      checked={!!srv.hibernation?.proxy}
                      onChange={(e) => saveHib({ proxy: e.target.checked })}
                    />
                    &nbsp;transparent proxy (TCP+UDP, no reconnect)
                  </label>
                  {!tcpOnly && !srv.hibernation?.proxy && (
                    <div className="error" style={{ fontSize: 12 }}>
                      UDP servers need the transparent proxy to auto-sleep.
                    </div>
                  )}
                </>
              )}
            </span>
          </div>
        )}
        {notice && <div className="notice">{notice}</div>}
        {error && <div className="error">{error}</div>}
      </div>
      <Schedules id={id} />
      {canManage && srv && <ServerSettings server={srv} onSaved={setSrv} />}
      {canManage && <Files id={id} />}
      {canManage && <SFTPCard id={id} initialEnabled={!!srv?.sftp?.enabled} username={user.username} />}
      {canManage && <Databases serverId={id} />}
      <Backups id={id} />
      {canManage && <Access id={id} />}
      {canManage && <Notifications serverId={id} />}
      <ServerAudit id={id} />
      <div className="card">
        <Console id={id} />
      </div>
    </>
  );
}

function SFTPCard({ id, initialEnabled, username }: { id: number; initialEnabled: boolean; username: string }) {
  const [enabled, setEnabled] = useState(initialEnabled);
  const [port, setPort] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function refresh() {
    try {
      const info = await api.sftpInfo(id);
      setEnabled(info.enabled);
      setPort(info.port);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    if (initialEnabled) refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function toggle() {
    setBusy(true);
    setError("");
    try {
      await api.setSFTP(id, !enabled);
      setEnabled(!enabled);
      if (!enabled) setTimeout(refresh, 1500); // give the controller a moment
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="card">
      <h2>SFTP</h2>
      <p className="muted">
        Access this server's files over SFTP using an SSH key from your{" "}
        <strong>Account → SSH keys</strong>. Available while the server is running.
      </p>
      <label className="row">
        <input type="checkbox" style={{ width: "auto" }} checked={enabled} disabled={busy} onChange={toggle} />
        &nbsp;Enable SFTP
      </label>
      {enabled && (
        <div style={{ marginTop: 8 }}>
          <div className="kv"><span className="k">Port</span><span>{port > 0 ? port : "provisioning…"}</span></div>
          <div className="kv"><span className="k">Username</span><span>{username}</span></div>
          {port > 0 && (
            <div className="kv">
              <span className="k">Connect</span>
              <code>sftp -P {port} {username}@&lt;node-ip&gt;</code>
            </div>
          )}
          <button onClick={refresh} style={{ marginTop: 8 }}>Refresh</button>
        </div>
      )}
      {error && <div className="error">{error}</div>}
    </div>
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
