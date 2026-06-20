import { useEffect, useState } from "react";
import { api, ApiError, PowerAction, Server } from "../api";
import { Console } from "./Console";

export function ServerDetail({ id, onBack }: { id: number; onBack: () => void }) {
  const [srv, setSrv] = useState<Server | null>(null);
  const [error, setError] = useState("");
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
    };
    load();
    const t = setInterval(load, 4000);
    return () => {
      active = false;
      clearInterval(t);
    };
  }, [id]);

  async function power(action: PowerAction) {
    setBusy(action);
    setError("");
    try {
      await api.power(id, action);
      setSrv(await api.server(id));
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
        <div className="kv">
          <span className="k">Endpoints</span>
          <span>{(srv.status.endpoints || []).join(", ") || "—"}</span>
        </div>
        {srv.status.message && (
          <div className="kv">
            <span className="k">Message</span>
            <span>{srv.status.message}</span>
          </div>
        )}
        <div className="row" style={{ marginTop: 12 }}>
          <button className="primary" disabled={busy !== ""} onClick={() => power("start")}>
            Start
          </button>
          <button disabled={busy !== ""} onClick={() => power("stop")}>
            Stop
          </button>
          <button disabled={busy !== ""} onClick={() => power("restart")}>
            Restart
          </button>
          <button className="danger" disabled={busy !== ""} onClick={() => power("kill")}>
            Kill
          </button>
        </div>
        {error && <div className="error">{error}</div>}
      </div>
      <div className="card">
        <Console id={id} />
      </div>
    </>
  );
}
