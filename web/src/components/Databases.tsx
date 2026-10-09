import { useId, FormEvent, useEffect, useState } from "react";
import { api, ApiError, DatabaseImport, ServerDatabase } from "../api";
import { useT } from "../i18n";

// Databases lists and provisions a server's databases (a schema + scoped user on
// a registered host). Credentials are shown on demand. canImport: the user may
// also load an SQL file of the server's into one -- it reads the server's
// files, so it takes the files permission too, as the API has it.
export function Databases({ serverId, canImport = false }: { serverId: number; canImport?: boolean }) {
  const { t } = useT();
  const [dbs, setDbs] = useState<ServerDatabase[]>([]);
  const [hosts, setHosts] = useState<{ id: number; name: string; kind: string; full: boolean }[]>([]);
  const [hostId, setHostId] = useState<number>(0);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  // Revealed credentials per database id (password fetched on demand).
  const [reveal, setReveal] = useState<Record<number, ServerDatabase>>({});
  // The database whose import form is open.
  const [importing, setImporting] = useState<number | null>(null);

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
  // An import under way is followed until it ends.
  const inFlight = dbs.some((d) => d.lastImport && (d.lastImport.phase === "Pending" || d.lastImport.phase === "Running"));
  useEffect(() => {
    if (!inFlight) return;
    const timer = setInterval(() => api.serverDatabases(serverId).then(setDbs).catch(() => {}), 4000);
    return () => clearInterval(timer);
  }, [inFlight, serverId]);

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
    if (!window.confirm(t('Rotate the password for "{name}"? Anything using the old password will stop working.', { name: d.databaseName }))) return;
    try {
      const rotated = await api.rotateServerDatabase(serverId, d.id);
      setReveal((m) => ({ ...m, [d.id]: rotated }));
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function remove(d: ServerDatabase) {
    if (!window.confirm(t('Delete database "{name}"? This drops the database and its data.', { name: d.databaseName }))) return;
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

  async function cancelImport(d: ServerDatabase) {
    setError("");
    try {
      await api.cancelDatabaseImport(serverId, d.id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <div className="card">
      <h2>{t("Databases")}</h2>
      {dbs.length === 0 ? (
        <p className="muted">{t("No databases yet.")}</p>
      ) : (
        dbs.map((d) => {
          const r = reveal[d.id];
          const imp = d.lastImport;
          const importBusy = !!imp && (imp.phase === "Pending" || imp.phase === "Running");
          return (
            <div key={d.id} className="card" style={{ background: "var(--surface-sunken)", marginBottom: 8 }}>
              <div className="kv"><span className="k">{t("Database")}</span><span><code>{d.databaseName}</code></span></div>
              <div className="kv"><span className="k">{t("Username")}</span><span><code>{d.username}</code></span></div>
              <div className="kv"><span className="k">{t("Endpoint")}</span><span><code>{d.host}:{d.port}</code>{d.hostName ? ` (${d.hostName})` : ""}</span></div>
              {r?.password && <div className="kv"><span className="k">{t("Password")}</span><span><code>{r.password}</code></span></div>}
              {imp && <ImportStatus imp={imp} />}
              <div className="row" style={{ marginTop: 8 }}>
                {!r?.password && <button onClick={() => show(d)}>{t("Show password")}</button>}
                <button onClick={() => rotate(d)}>{t("Rotate password")}</button>
                {canImport && !importBusy && importing !== d.id && <button onClick={() => setImporting(d.id)}>{t("Import SQL…")}</button>}
                {imp?.phase === "Pending" && <button onClick={() => cancelImport(d)}>{t("Cancel the import")}</button>}
                <button className="danger" disabled={importBusy} onClick={() => remove(d)}>{t("Delete")}</button>
              </div>
              {importing === d.id && (
                <ImportForm
                  serverId={serverId}
                  db={d}
                  onClose={() => setImporting(null)}
                  onQueued={async () => {
                    setImporting(null);
                    await load();
                  }}
                />
              )}
            </div>
          );
        })
      )}

      <div className="row" style={{ marginTop: 12 }}>
        {hosts.length === 0 ? (
          <span className="muted">{t("No database hosts are configured. Ask an admin to add one.")}</span>
        ) : (
          <>
            <select aria-label={t("Database host")} value={hostId} onChange={(e) => setHostId(Number(e.target.value))}>
              {hosts.map((h) => (
                <option key={h.id} value={h.id} disabled={h.full}>
                  {h.name} ({h.kind}){h.full ? t(" — full") : ""}
                </option>
              ))}
            </select>
            <button className="primary" onClick={create} disabled={busy || !hostId}>
              {busy ? t("Creating…") : t("Create database")}
            </button>
          </>
        )}
      </div>
      <p className="muted">{t("A server's backups take its databases with its files: each backup holds a dump of them.")}</p>
      {error && <div className="error">{error}</div>}
    </div>
  );
}

// ImportStatus says how the database's last import went.
function ImportStatus({ imp }: { imp: DatabaseImport }) {
  const { t } = useT();
  const cls = imp.phase === "Succeeded" ? "Running" : imp.phase === "Failed" ? "Crashed" : "Starting";
  const when = new Date(imp.completedAt ?? imp.createdAt).toLocaleString();
  return (
    <div className="kv">
      <span className="k">{t("Last import")}</span>
      <span>
        <span className={`badge ${cls}`}>{t(imp.phase)}</span> <code>{imp.path}</code> <span className="muted">{when}</span>
        {imp.phase === "Pending" && <div className="muted" style={{ fontSize: 12 }}>{t("Waiting for the server to be stopped. It cannot be started until the import is done.")}</div>}
        {imp.message && <div className={imp.phase === "Failed" ? "error" : "muted"} style={{ fontSize: 12 }}>{imp.message}</div>}
      </span>
    </div>
  );
}

// ImportForm loads an SQL file of the server's into the database. The file is
// uploaded first, with the file manager or SFTP; the .sql and .gz files at the
// top of the server's files are offered.
function ImportForm({ serverId, db, onClose, onQueued }: { serverId: number; db: ServerDatabase; onClose: () => void; onQueued: () => void }) {
  const fieldId = useId();
  const { t } = useT();
  const [path, setPath] = useState("");
  const [wipe, setWipe] = useState(true);
  const [found, setFound] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    api
      .listFiles(serverId, "/")
      .then((fs) => {
        const sql = fs.filter((f) => !f.dir && /\.(sql|sql\.gz|gz)$/i.test(f.name)).map((f) => "/" + f.name);
        setFound(sql);
        if (sql.length === 1) setPath(sql[0]);
      })
      .catch(() => {});
  }, [serverId]);

  async function submit(e: FormEvent) {
    e.preventDefault();
    const warning = wipe
      ? t('Load {path} into "{name}"? The database is emptied first: everything it holds now is replaced by the file.', { path, name: db.databaseName })
      : t('Load {path} into "{name}", on top of what it holds?', { path, name: db.databaseName });
    if (!window.confirm(warning)) return;
    setBusy(true);
    setError("");
    try {
      await api.importDatabase(serverId, db.id, path.trim(), wipe);
      onQueued();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  const listId = `${fieldId}-sql-files`;
  return (
    <form onSubmit={submit} style={{ marginTop: 12 }}>
      <label htmlFor={`${fieldId}-sql-file`}>{t("SQL file, from the server's files")}</label>
      <input aria-describedby={`${fieldId}-sql-help`} id={`${fieldId}-sql-file`} value={path} list={listId} onChange={(e) => setPath(e.target.value)} placeholder="/dump.sql" required />
      <datalist id={listId}>
        {found.map((f) => (
          <option key={f} value={f} />
        ))}
      </datalist>
      <p id={`${fieldId}-sql-help`} className="muted">
        {t("Upload it first with the file manager or SFTP: a dump of one database (mysqldump, mariadb-dump), plain or gzipped. Lines that switch to another database and the definers of a dump made as root are left out, so a dump taken elsewhere fits.")}
      </p>
      <label className="row">
        <input aria-describedby={`${fieldId}-import-help`} type="checkbox" style={{ width: "auto" }} checked={wipe} onChange={(e) => setWipe(e.target.checked)} />
        &nbsp;{t("Empty the database first")}
      </label>
      <p id={`${fieldId}-import-help`} className="muted">{t("The server has to be stopped: the import waits for it, and the server cannot be started until the import is done.")}</p>
      {error && <div className="error">{error}</div>}
      <div className="row" style={{ gap: 8, marginTop: 8 }}>
        <button className="primary" disabled={busy || !path.trim()}>{busy ? t("Queuing…") : t("Import")}</button>
        <button type="button" onClick={onClose}>{t("Cancel")}</button>
      </div>
    </form>
  );
}
