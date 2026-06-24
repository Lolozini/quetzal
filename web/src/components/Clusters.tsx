import { FormEvent, useEffect, useState } from "react";
import { api, ApiError, Cluster, ClusterNode } from "../api";
import { useT } from "../i18n";

export function Clusters() {
  const { t } = useT();
  const [clusters, setClusters] = useState<Cluster[]>([]);
  const [error, setError] = useState("");
  const [name, setName] = useState("");
  const [kubeconfig, setKubeconfig] = useState("");
  const [busy, setBusy] = useState(false);
  const [nodesFor, setNodesFor] = useState<number | null>(null);
  const [nodes, setNodes] = useState<ClusterNode[]>([]);

  async function load() {
    try {
      setClusters(await api.clusters());
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
      await api.createCluster(name, kubeconfig);
      setName("");
      setKubeconfig("");
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function test(c: Cluster) {
    setError("");
    try {
      await api.testCluster(c.id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function remove(c: Cluster) {
    if (!window.confirm(t('Remove cluster "{name}"? Servers on it must be deleted first.', { name: c.name }))) return;
    setError("");
    try {
      await api.deleteCluster(c.id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function showNodes(c: Cluster) {
    if (nodesFor === c.id) {
      setNodesFor(null);
      return;
    }
    setError("");
    try {
      setNodes(await api.clusterNodes(c.id));
      setNodesFor(c.id);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <div className="card">
      <h2>{t("Clusters")}</h2>
      <p className="muted">
        {t("Deploy targets. The local cluster is the one the control plane runs in; add remote clusters by registering their kubeconfig (stored encrypted).")}
      </p>
      <table>
        <thead>
          <tr><th>{t("Name")}</th><th>{t("Type")}</th><th>{t("Status")}</th><th>{t("Nodes")}</th><th></th></tr>
        </thead>
        <tbody>
          {clusters.map((c) => (
            <tr key={c.id}>
              <td>{c.name}<div className="muted" style={{ fontSize: 12 }}>{c.slug}</div></td>
              <td>{c.inCluster ? t("local") : t("remote")}</td>
              <td>
                <span className={`badge ${c.reachable ? "Running" : "Crashed"}`}>
                  {c.reachable ? t("reachable") : t("unreachable")}
                </span>
                {c.version && <span className="muted" style={{ fontSize: 12 }}> {c.version}</span>}
                {c.statusMessage && <div className="muted" style={{ fontSize: 12 }} title={c.statusMessage}>{c.statusMessage.slice(0, 60)}</div>}
              </td>
              <td>{c.nodeCount ?? "—"}</td>
              <td style={{ whiteSpace: "nowrap" }}>
                <button onClick={() => test(c)}>{t("Test")}</button>{" "}
                <button onClick={() => showNodes(c)}>{nodesFor === c.id ? t("Hide") : t("Nodes")}</button>{" "}
                {!c.inCluster && <button className="danger" onClick={() => remove(c)}>{t("Remove")}</button>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {nodesFor !== null && (
        <table style={{ marginTop: 8 }}>
          <thead>
            <tr><th>{t("Node")}</th><th>{t("Ready")}</th><th>{t("Version")}</th><th>{t("CPU")}</th><th>{t("Memory")}</th><th>{t("IP")}</th></tr>
          </thead>
          <tbody>
            {nodes.length === 0 ? (
              <tr><td colSpan={6} className="muted">{t("No nodes (or not listable).")}</td></tr>
            ) : (
              nodes.map((n) => (
                <tr key={n.name}>
                  <td>{n.name}</td>
                  <td>{n.ready ? "✓" : "✗"}</td>
                  <td>{n.version}</td>
                  <td>{n.cpu}</td>
                  <td>{n.memory}</td>
                  <td>{n.internalIP || "—"}</td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      )}

      <form onSubmit={add} style={{ marginTop: 12 }}>
        <h3>{t("Register a remote cluster")}</h3>
        <label>{t("Name")}</label>
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="edge-1" required />
        <label>{t("Kubeconfig (YAML)")}</label>
        <textarea
          value={kubeconfig}
          onChange={(e) => setKubeconfig(e.target.value)}
          rows={8}
          style={{ width: "100%", fontFamily: "monospace", fontSize: 12 }}
          placeholder="apiVersion: v1&#10;kind: Config&#10;clusters: ..."
          required
        />
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={busy || !name || !kubeconfig}>
          {busy ? t("Registering…") : t("Register cluster")}
        </button>
      </form>
    </div>
  );
}
