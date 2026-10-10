import { useId, FormEvent, useEffect, useState } from "react";
import { ALL_PERMISSIONS, api, ServerAccess, ServerInvite, errorMessage } from "../api";
import { useT } from "../i18n";

// What each permission allows. The names alone left it to guess that "view"
// also lists the server's backups and schedules, while its files, console and
// databases each need their own.
export function permissionHelp(t: ReturnType<typeof useT>["t"], p: (typeof ALL_PERMISSIONS)[number]): string {
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
  const fieldId = useId();
  const { t } = useT();
  const [list, setList] = useState<ServerAccess[]>([]);
  const [invites, setInvites] = useState<ServerInvite[]>([]);
  // A username grants at once; an email address sends an invitation, which
  // whoever reads that mailbox accepts from their account or a new one.
  const [who, setWho] = useState("");
  const [perms, setPerms] = useState<string[]>(["view"]);
  const [error, setError] = useState("");
  const [msg, setMsg] = useState("");
  const [busy, setBusy] = useState(false);
  const byEmail = who.includes("@");

  async function load() {
    try {
      const [a, i] = await Promise.all([api.access(id), api.invites(id)]);
      setList(a);
      setInvites(i);
    } catch (e) {
      setError(errorMessage(e, t));
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
    setMsg("");
    try {
      const target = who.trim();
      if (byEmail) {
        await api.invite(id, target, perms);
        setMsg(t("Invitation sent to {email}. The link is good for 7 days.", { email: target }));
      } else {
        await api.grantAccess(id, target, perms);
      }
      setWho("");
      setPerms(["view"]);
      await load();
    } catch (err) {
      setError(errorMessage(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function revoke(a: ServerAccess) {
    if (!window.confirm(t("Revoke {name}'s access?", { name: a.username ?? "" }))) return;
    await api.revokeAccess(id, a.userId).catch((e) => setError(errorMessage(e, t)));
    await load();
  }

  async function withdraw(inv: ServerInvite) {
    if (!window.confirm(t("Withdraw the invitation to {email}?", { email: inv.email }))) return;
    await api.withdrawInvite(id, inv.id).catch((e) => setError(errorMessage(e, t)));
    await load();
  }

  return (
    <div className="card">
      <h3>{t("Subusers")}</h3>
      {list.length === 0 ? (
        <p className="muted">{t("No subusers. Grant another account scoped access below, or invite someone by email.")}</p>
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
      {invites.length > 0 && (
        <>
          <h4 style={{ marginTop: 16 }}>{t("Invitations waiting")}</h4>
          <div className="table-scroll">
            <table>
              <thead><tr><th>{t("Email")}</th><th>{t("Permissions")}</th><th>{t("Expires")}</th><th></th></tr></thead>
              <tbody>
                {invites.map((inv) => (
                  <tr key={inv.id}>
                    <td>{inv.email}</td>
                    <td>{inv.permissions.join(", ")}</td>
                    <td>{new Date(inv.expiresAt).toLocaleDateString()}</td>
                    <td><button className="danger" onClick={() => withdraw(inv)}>{t("Withdraw")}</button></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
      <form onSubmit={grant} style={{ marginTop: 12 }}>
        <label htmlFor={`${fieldId}-recipient`}>{t("Username or email address")}</label>
        <input aria-describedby={`${fieldId}-recipient-help`} id={`${fieldId}-recipient`} value={who} onChange={(e) => setWho(e.target.value)} placeholder={t("an account, or an address to invite")} required />
        <p id={`${fieldId}-recipient-help`} className="muted" style={{ marginTop: 4 }}>
          {byEmail
            ? t("They get a link by email, to accept from their account or a new one.")
            : t("An account on this panel gets access at once.")}
        </p>
        <div id={`${fieldId}-permissions-label`} style={{ display: "block", margin: "10px 0 4px", color: "var(--ink-muted)", fontSize: 13 }}>{t("Permissions")}</div>
        <div role="group" aria-labelledby={`${fieldId}-permissions-label`} className="perm-list">
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
        {msg && <div className="notice">{msg}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={busy || !who.trim() || perms.length === 0}>
          {busy ? (byEmail ? t("Sending…") : t("Granting…")) : byEmail ? t("Send invitation") : t("Grant access")}
        </button>
      </form>
    </div>
  );
}
