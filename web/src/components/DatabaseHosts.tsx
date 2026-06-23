import { FormEvent, useEffect, useState } from "react";
import { api, ApiError, DatabaseHost } from "../api";

// DatabaseHosts is the admin registry of MySQL/MariaDB hosts: external servers
// the panel provisions on, or MariaDB instances Quetzal deploys and manages.
export function DatabaseHosts() {
  const [hosts, setHosts] = useState<DatabaseHost[]>([]);
  const [error, setError] = useState("");
  const [kind, setKind] = useState<"external" | "managed">("external");
  const [form, setForm] = useState({
    name: "", host: "", port: 3306, adminUser: "root", adminPassword: "",
    connectHost: "", connectPort: 0, maxDatabases: 0,
    namespace: "", image: "", storageSize: "1Gi",
  });
  const [busy, setBusy] = useState(false);

  async function load() {
    try {
      setHosts(await api.databaseHosts());
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, []);

  const set = (k: keyof typeof form) => (e: { target: { value: string } }) =>
    setForm({ ...form, [k]: k === "port" || k === "connectPort" || k === "maxDatabases" ? Number(e.target.value) : e.target.value });

  async function add(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const body: Record<string, unknown> = {
        name: form.name, kind, connectHost: form.connectHost, connectPort: form.connectPort, maxDatabases: form.maxDatabases,
      };
      if (kind === "external") {
        Object.assign(body, { host: form.host, port: form.port, adminUser: form.adminUser, adminPassword: form.adminPassword });
      } else {
        Object.assign(body, { namespace: form.namespace, image: form.image, storageSize: form.storageSize });
      }
      await api.createDatabaseHost(body);
      setForm({ ...form, name: "", host: "", adminPassword: "", connectHost: "", namespace: "" });
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function test(h: DatabaseHost) {
    setError("");
    try {
      await api.testDatabaseHost(h.id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function remove(h: DatabaseHost) {
    if (!window.confirm(`Delete database host "${h.name}"?`)) return;
    setError("");
    try {
      await api.deleteDatabaseHost(h.id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <div className="card">
      <h2>Database hosts</h2>
      <p className="muted">
        MySQL/MariaDB servers the panel provisions databases on. Register an{" "}
        <strong>external</strong> server, or have Quetzal deploy and manage a{" "}
        <strong>MariaDB</strong> in-cluster.
      </p>
      {hosts.length > 0 && (
        <table>
          <thead>
            <tr><th>Name</th><th>Kind</th><th>Address</th><th>DBs</th><th>Status</th><th></th></tr>
          </thead>
          <tbody>
            {hosts.map((h) => (
              <tr key={h.id}>
                <td>{h.name}</td>
                <td>{h.kind}</td>
                <td><code>{h.host}:{h.port}</code></td>
                <td>{h.databases ?? 0}{h.maxDatabases ? ` / ${h.maxDatabases}` : ""}</td>
                <td>
                  {h.reachable ? <span className="badge Running">reachable</span> : <span className="muted" title={h.statusMessage}>unknown</span>}
                </td>
                <td style={{ whiteSpace: "nowrap" }}>
                  <button onClick={() => test(h)}>Test</button>{" "}
                  <button className="danger" onClick={() => remove(h)}>Delete</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <form onSubmit={add} style={{ marginTop: 12 }}>
        <h3>Add a host</h3>
        <div className="grid2">
          <div><label>Name</label><input value={form.name} onChange={set("name")} required /></div>
          <div>
            <label>Kind</label>
            <select value={kind} onChange={(e) => setKind(e.target.value as "external" | "managed")}>
              <option value="external">External (existing server)</option>
              <option value="managed">Managed (Quetzal deploys MariaDB)</option>
            </select>
          </div>
        </div>
        {kind === "external" ? (
          <>
            <div className="grid2">
              <div><label>Host</label><input value={form.host} onChange={set("host")} placeholder="db.example.com" required /></div>
              <div><label>Port</label><input type="number" value={form.port} onChange={set("port")} /></div>
            </div>
            <div className="grid2">
              <div><label>Admin user</label><input value={form.adminUser} onChange={set("adminUser")} autoComplete="off" /></div>
              <div><label>Admin password</label><input type="password" value={form.adminPassword} onChange={set("adminPassword")} autoComplete="new-password" required /></div>
            </div>
            <div className="grid2">
              <div><label>Advertised host (optional)</label><input value={form.connectHost} onChange={set("connectHost")} placeholder="defaults to host" /></div>
              <div><label>Advertised port (optional)</label><input type="number" value={form.connectPort} onChange={set("connectPort")} /></div>
            </div>
          </>
        ) : (
          <>
            <p className="muted">Quetzal deploys a MariaDB (Deployment + PVC + Service) on the local cluster and owns the root password. Game servers reach it via the in-cluster DNS name.</p>
            <div className="grid2">
              <div><label>Image</label><input value={form.image} onChange={set("image")} placeholder="mariadb:11.4" /></div>
              <div><label>Storage size</label><input value={form.storageSize} onChange={set("storageSize")} placeholder="1Gi" /></div>
            </div>
            <div><label>Namespace (optional)</label><input value={form.namespace} onChange={set("namespace")} placeholder="quetzal-db-<id>" /></div>
          </>
        )}
        <div><label>Max databases (0 = ∞)</label><input type="number" min={0} value={form.maxDatabases} onChange={set("maxDatabases")} /></div>
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={busy || !form.name}>
          {busy ? "Adding…" : "Add host"}
        </button>
      </form>
    </div>
  );
}
