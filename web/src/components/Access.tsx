import { FormEvent, useEffect, useState } from "react";
import { ALL_PERMISSIONS, api, ApiError, ServerAccess } from "../api";
import { useT } from "../i18n";

// What each permission allows. The names alone left it to guess that "view"
// also lists the server's backups and schedules, while its files, console and
// databases each need their own.
function permissionHelp(t: ReturnType<typeof useT>["t"], p: (typeof ALL_PERMISSIONS)[number]): string {
  switch (p) {
    case "view":
      return t("its page: state, address, usage, backups, schedules and activity");
    case "power":
      return t("start, stop, restart and kill it");
    case "console":
      return t("its live console and setup log, commands included");
    case "schedules":
      return t("its scheduled tasks, within the other permissions");
    case "backups":
      return t("take, restore and delete backups");
    case "files":
      return t("its files, from the panel and over SFTP");
    case "settings":
      return t("its variables, resources, ports, exposure, hibernation, reinstall");
    case "databases":
      return t("its databases and their passwords");
    case "delete":
      return t("delete it");
  }
}

export function Access({ id }: { id: number }) {
  const { t } = useT();
  const [list, setList] = useState<ServerAccess[]>([]);
  const [username, setUsername] = useState("");
  const [perms, setPerms] = useState<string[]>(["view"]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function load() {
    try {
      setList(await api.access(id));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, [id]);

  function toggle(p: string) {
    setPerms((cur) => (cur.includes(p) ? cur.filter((x) => x !== p) : [...cur, p]));
  }

  async function grant(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.grantAccess(id, username, perms);
      setUsername("");
      setPerms(["view"]);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function revoke(a: ServerAccess) {
    if (!window.confirm(t("Revoke {name}'s access?", { name: a.username ?? "" }))) return;
    await api.revokeAccess(id, a.userId).catch((e) => setError(String(e)));
    await load();
  }

  return (
    <div className="card">
      <h3>{t("Subusers")}</h3>
      {list.length === 0 ? (
        <p className="muted">{t("No subusers. Grant another account scoped access below.")}</p>
      ) : (
        <div className="table-scroll">
          <table>
            <thead><tr><th>{t("User")}</th><th>{t("Permissions")}</th><th></th></tr></thead>
            <tbody>
              {list.map((a) => (
                <tr key={a.id}>
                  <td>{a.username}</td>
                  <td>{a.permissions.join(", ")}</td>
                  <td><button className="danger" onClick={() => revoke(a)}>{t("Revoke")}</button></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <form onSubmit={grant} style={{ marginTop: 12 }}>
        <label>{t("Username")}</label>
        <input value={username} onChange={(e) => setUsername(e.target.value)} placeholder={t("existing account")} required />
        <label>{t("Permissions")}</label>
        <div className="perm-list">
          {ALL_PERMISSIONS.map((p) => (
            <label key={p}>
              <input type="checkbox" checked={perms.includes(p)} onChange={() => toggle(p)} />
              <span>
                <b>{p}</b> <span className="muted">{permissionHelp(t, p)}</span>
              </span>
            </label>
          ))}
        </div>
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={busy || !username || perms.length === 0}>
          {busy ? t("Granting…") : t("Grant access")}
        </button>
      </form>
    </div>
  );
}
