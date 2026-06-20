import { useEffect, useState } from "react";
import { api, Server } from "../api";

export function ServerList({
  onCreate,
  onOpen,
}: {
  onCreate: () => void;
  onOpen: (id: number) => void;
}) {
  const [servers, setServers] = useState<Server[]>([]);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    const load = async () => {
      try {
        const s = await api.servers();
        if (active) setServers(s);
      } catch (e) {
        if (active) setError(String(e));
      }
    };
    load();
    const t = setInterval(load, 5000);
    return () => {
      active = false;
      clearInterval(t);
    };
  }, []);

  return (
    <div className="card">
      <div className="row">
        <h2>Servers</h2>
        <div className="spacer" />
        <button className="primary" onClick={onCreate}>
          + New server
        </button>
      </div>
      {error && <div className="error">{error}</div>}
      {servers.length === 0 ? (
        <p className="muted">No servers yet. Create one to get started.</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Desired</th>
              <th>Phase</th>
              <th>Endpoints</th>
            </tr>
          </thead>
          <tbody>
            {servers.map((s) => (
              <tr key={s.id} className="clickable" onClick={() => onOpen(s.id)}>
                <td>{s.displayName}</td>
                <td>
                  <span className={`badge ${s.desiredState}`}>{s.desiredState}</span>
                </td>
                <td>
                  <span className={`badge ${s.status.phase}`}>{s.status.phase}</span>
                </td>
                <td className="muted">{(s.status.endpoints || []).join(", ") || "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
