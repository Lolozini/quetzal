import { useCallback, useEffect, useId, useRef, useState } from "react";
import { api, Server, errorMessage } from "../api";
import { useT } from "../i18n";

export function ServerList({
  onCreate,
  onOpen,
  canCreate,
  onUserRefresh,
}: {
  onCreate: () => void;
  onOpen: (id: number) => void;
  canCreate: boolean;
  onUserRefresh: () => Promise<void>;
}) {
  const { t } = useT();
  const creationHelpId = useId();
  const [servers, setServers] = useState<Server[]>([]);
  const [error, setError] = useState<unknown>(null);
  const [query, setQuery] = useState("");
  // Debounced so typing does not fire a request per keystroke; the list also
  // polls, so the query has to be part of what the poll sends.
  const [search, setSearch] = useState("");
  useEffect(() => {
    const h = setTimeout(() => setSearch(query.trim()), 250);
    return () => clearTimeout(h);
  }, [query]);

  useEffect(() => {
    let active = true;
    const load = async () => {
      try {
        const s = await api.servers(search);
        if (active) setServers(s);
      } catch (e) {
        if (active) setError(e);
      }
    };
    load();
    const timer = setInterval(load, 5000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, [search]);

  return (
    <div className="card">
      <div className="row">
        <h2>{t("Servers")}</h2>
        <div className="spacer" />
        <input aria-label={t("Search servers")}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder={t("Search servers")}
          style={{ width: "auto", maxWidth: 220 }}
        />
        <button className="primary" onClick={onCreate} disabled={!canCreate} aria-describedby={!canCreate ? creationHelpId : undefined}>
          + {t("New server")}
        </button>
      </div>
      {!canCreate && <CreationRestriction id={creationHelpId} onRefresh={onUserRefresh} />}
      {!!error && <div className="error">{errorMessage(error, t)}</div>}
      {servers.length === 0 ? (
        <p className="muted">
          {search ? t("No server matches that search.") : canCreate ? t("No servers yet. Create one to get started.") : t("No servers yet.")}
        </p>
      ) : (
        <div className="table-scroll">
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
                  <td>
                    <a href={`#/servers/${s.id}`} onClick={(e) => e.stopPropagation()}>
                      {s.displayName}
                    </a>
                  </td>
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
        </div>
      )}
    </div>
  );
}

// A denied create route can outlive an administrator granting the account a
// quota. Refresh on entry and offer an explicit recheck without a page reload.
export function CreationRestriction({ id, onRefresh }: { id?: string; onRefresh: () => Promise<void> }) {
  const { t } = useT();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const checking = useRef(false);
  const check = useCallback(async () => {
    if (checking.current) return;
    checking.current = true;
    setBusy(true);
    setError(null);
    try {
      await onRefresh();
    } catch (err) {
      setError(err);
    } finally {
      checking.current = false;
      setBusy(false);
    }
  }, [onRefresh]);
  useEffect(() => { void check(); }, [check]);
  return (
    <div className="notice">
      <p id={id}>{t("Your account cannot create servers. Ask an administrator to enable creation.")}</p>
      <button type="button" onClick={check} disabled={busy}>
        {busy ? t("Checking…") : t("Check access again")}
      </button>
      {!!error && <p className="error" role="alert">{errorMessage(error, t)}</p>}
    </div>
  );
}
