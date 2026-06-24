import { useEffect, useState } from "react";
import { api, Server } from "../api";
import { useT } from "../i18n";

export function ServerList({
  onCreate,
  onOpen,
}: {
  onCreate: () => void;
  onOpen: (id: number) => void;
}) {
  const { t } = useT();
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
        <h2>{t("Servers")}</h2>
        <div className="spacer" />
        <button className="primary" onClick={onCreate}>
          + {t("New server")}
        </button>
      </div>
      {error && <div className="error">{error}</div>}
      {servers.length === 0 ? (
        <p className="muted">{t("No servers yet. Create one to get started.")}</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>{t("Name")}</th>
              <th>{t("Desired")}</th>
              <th>{t("Phase")}</th>
              <th>{t("Endpoints")}</th>
            </tr>
          </thead>
          <tbody>
            {servers.map((s) => (
              <tr key={s.id} className="clickable" onClick={() => onOpen(s.id)}>
                <td>{s.displayName}</td>
                <td>
                  <span className={`badge ${s.desiredState}`}>{t(s.desiredState)}</span>
                </td>
                <td>
                  <span className={`badge ${s.status.phase}`}>{t(s.status.phase)}</span>
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
