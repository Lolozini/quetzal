import { FormEvent, useEffect, useState } from "react";
import { api, ApiError, Backup, BackupConfig, BackupConfigInput } from "../api";
import { useT } from "../i18n";

export function Backups({ id }: { id: number }) {
  const { t } = useT();
  const [cfg, setCfg] = useState<BackupConfig | null>(null);
  const [list, setList] = useState<Backup[]>([]);
  const [error, setError] = useState("");
  const [showCfg, setShowCfg] = useState(false);
  const [busy, setBusy] = useState("");

  async function load() {
    try {
      const [c, bs] = await Promise.all([api.backupConfig(), api.backups(id)]);
      setCfg(c);
      setList(bs);
      if (!c.configured) setShowCfg(true);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
    const t = setInterval(() => api.backups(id).then(setList).catch(() => {}), 4000);
    return () => clearInterval(t);
  }, [id]);

  async function backupNow() {
    setBusy("backup");
    setError("");
    try {
      await api.createBackup(id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy("");
    }
  }

  async function restore(b: Backup) {
    if (!window.confirm(t("Restore this backup into the server's volume? Current data will be overwritten by the snapshot.\n\nThe server must be stopped first (a live restore would corrupt the data)."))) return;
    setError("");
    try {
      await api.restoreBackup(id, b.id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function remove(b: Backup) {
    if (!window.confirm(t("Delete this backup record?"))) return;
    try {
      await api.deleteBackup(id, b.id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <div className="card">
      <div className="row">
        <h3>{t("Backups")}</h3>
        <div className="spacer" />
        <button onClick={() => setShowCfg((v) => !v)}>{showCfg ? t("Hide target") : t("Backup target")}</button>
        <button className="primary" disabled={busy !== "" || !cfg?.configured} onClick={backupNow}>
          {busy === "backup" ? t("Queuing…") : t("Backup now")}
        </button>
      </div>

      {cfg && !cfg.configured && (
        <p className="muted">{t("No backup target configured yet — set one below to enable backups.")}</p>
      )}

      {showCfg && <BackupConfigForm cfg={cfg} onSaved={load} />}

      {list.length === 0 ? (
        <p className="muted">{t("No backups yet.")}</p>
      ) : (
        <table>
          <thead>
            <tr><th>#</th><th>{t("Type")}</th><th>{t("Status")}</th><th>{t("Size")}</th><th>{t("When")}</th><th></th></tr>
          </thead>
          <tbody>
            {list.map((b) => (
              <tr key={b.id}>
                <td>{b.id}</td>
                <td>{b.direction}{b.direction === "restore" && b.sourceId ? ` ←#${b.sourceId}` : ""}</td>
                <td title={b.message}><span className={`badge ${phaseClass(b.phase)}`}>{t(b.phase)}</span></td>
                <td>{b.sizeBytes ? fmtBytes(b.sizeBytes) : "—"}</td>
                <td>{new Date(b.createdAt).toLocaleString()}</td>
                <td style={{ whiteSpace: "nowrap" }}>
                  {b.direction === "backup" && b.phase === "Succeeded" && (
                    <button onClick={() => restore(b)}>{t("Restore")}</button>
                  )}{" "}
                  <button className="danger" onClick={() => remove(b)}>{t("Delete")}</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {error && <div className="error">{error}</div>}
    </div>
  );
}

function BackupConfigForm({ cfg, onSaved }: { cfg: BackupConfig | null; onSaved: () => void }) {
  const { t } = useT();
  const [f, setF] = useState<BackupConfigInput>({
    endpoint: cfg?.endpoint ?? "",
    bucket: cfg?.bucket ?? "",
    prefix: cfg?.prefix ?? "",
    region: cfg?.region ?? "",
    useSSL: cfg?.useSSL ?? true,
    keepLast: cfg?.keepLast ?? 7,
    runnerImage: cfg?.runnerImage ?? "",
  });
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  const set = (k: keyof BackupConfigInput, v: string | number | boolean) => setF({ ...f, [k]: v });

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    setSaved(false);
    try {
      await api.setBackupConfig(f);
      setSaved(true);
      onSaved();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} style={{ borderBottom: "1px solid var(--border)", paddingBottom: 12, marginBottom: 12 }}>
      <div className="grid2">
        <div><label>{t("S3 endpoint (host:port)")}</label>
          <input value={f.endpoint} onChange={(e) => set("endpoint", e.target.value)} placeholder="s3.gra.io.cloud.ovh.net" required /></div>
        <div><label>{t("Bucket")}</label>
          <input value={f.bucket} onChange={(e) => set("bucket", e.target.value)} placeholder="quetzal-backups" required /></div>
      </div>
      <div className="grid2">
        <div><label>{t("Prefix (optional)")}</label>
          <input value={f.prefix} onChange={(e) => set("prefix", e.target.value)} placeholder="games" /></div>
        <div><label>{t("Region (optional)")}</label>
          <input value={f.region} onChange={(e) => set("region", e.target.value)} placeholder="gra" /></div>
      </div>
      <div className="grid2">
        <div><label>{t("Keep last (snapshots)")}</label>
          <input type="number" min={1} value={f.keepLast} onChange={(e) => set("keepLast", Number(e.target.value))} /></div>
        <div><label>{t("Runner image (optional)")}</label>
          <input value={f.runnerImage} onChange={(e) => set("runnerImage", e.target.value)} placeholder="restic/restic:0.17.3" /></div>
      </div>
      <label className="row"><input type="checkbox" style={{ width: "auto" }} checked={f.useSSL} onChange={(e) => set("useSSL", e.target.checked)} />&nbsp;{t("Use TLS (https)")}</label>
      <div className="grid2" style={{ marginTop: 8 }}>
        <div><label>{t("Access key")} {cfg?.hasCredentials ? t("(set — leave blank to keep)") : ""}</label>
          <input value={f.accessKey ?? ""} autoComplete="off" onChange={(e) => set("accessKey", e.target.value)} /></div>
        <div><label>{t("Secret key")} {cfg?.hasCredentials ? t("(set — leave blank to keep)") : ""}</label>
          <input type="password" value={f.secretKey ?? ""} autoComplete="new-password" onChange={(e) => set("secretKey", e.target.value)} /></div>
      </div>
      <label>{t("Repository password")} {cfg?.hasPassword ? t("(set — leave blank to keep)") : t("(restic encryption key)")}</label>
      <input type="password" value={f.repoPassword ?? ""} autoComplete="new-password" onChange={(e) => set("repoPassword", e.target.value)} />
      {error && <div className="error">{error}</div>}
      {saved && <div className="notice">{t("Backup target saved.")}</div>}
      <button className="primary" style={{ marginTop: 12 }} disabled={busy}>{busy ? t("Saving…") : t("Save target")}</button>
    </form>
  );
}

function phaseClass(p: string): string {
  if (p === "Succeeded") return "Running";
  if (p === "Failed") return "Crashed";
  return "Starting";
}

function fmtBytes(n: number): string {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(2)} GiB`;
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MiB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(0)} KiB`;
  return `${n} B`;
}
