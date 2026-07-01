import { FormEvent, useEffect, useState } from "react";
import { AdminPermInfo, AdminRole, api, ApiError, AuditEntry, EmailSettingsInput, hasAdminPerm, NetworkSettings, User } from "../api";
import { useT } from "../i18n";
import { Collapsible } from "./Collapsible";
import { Clusters } from "./Clusters";
import { DatabaseHosts } from "./DatabaseHosts";
import { Notifications } from "./Notifications";
import { Templates } from "./Templates";

export function Admin({ user }: { user: User }) {
  const can = (p: string) => hasAdminPerm(user, p);
  return (
    <>
      {can("users") && <Users me={user} />}
      {user.isAdmin && <Roles />}
      {can("templates") && <Templates />}
      {can("settings") && <NetworkSettingsCard />}
      {can("settings") && <EmailSettingsCard />}
      {can("database-hosts") && <DatabaseHosts />}
      {can("clusters") && <Clusters />}
      {can("notifications") && <Notifications serverId={0} />}
      {can("audit") && <GlobalAudit />}
    </>
  );
}

// Users is shown to admins holding the "users" permission. Admin-status and
// admin-role controls are superadmin-only (me.isAdmin) — a scoped users-admin
// manages regular accounts but can't escalate privileges.
function Users({ me }: { me: User }) {
  const { t } = useT();
  const [users, setUsers] = useState<User[]>([]);
  const [roles, setRoles] = useState<AdminRole[]>([]);
  const [error, setError] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [email, setEmail] = useState("");
  const [isAdmin, setIsAdmin] = useState(false);
  const [maxServers, setMaxServers] = useState(0);
  const [maxMemoryMB, setMaxMemoryMB] = useState(0);
  const [busy, setBusy] = useState(false);

  async function load() {
    try {
      setUsers(await api.users());
      if (me.isAdmin) setRoles(await api.adminRoles());
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, []);

  // Describe a user's administrative standing for the Role column.
  function roleLabel(u: User): string {
    if (u.isAdmin) return t("superadmin");
    if (u.adminRoleId != null) {
      // roles are only loaded for superadmins; fall back to a generic label.
      const r = roles.find((x) => x.id === u.adminRoleId);
      return r ? t("admin: {role}", { role: r.name }) : t("scoped admin");
    }
    return t("user");
  }

  async function setAdminRole(u: User, roleId: number | null) {
    setError("");
    try {
      await api.setUserAdminRole(u.id, roleId);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function add(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.createUser({ username, password, email: email.trim(), isAdmin, maxServers, maxMemoryMB });
      setUsername("");
      setPassword("");
      setEmail("");
      setIsAdmin(false);
      setMaxServers(0);
      setMaxMemoryMB(0);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function toggleAdmin(u: User) {
    setError("");
    try {
      await api.updateUser(u.id, {
        isAdmin: !u.isAdmin,
        maxServers: u.maxServers ?? 0,
        maxMemoryMB: u.maxMemoryMB ?? 0,
        maxCpuMilli: u.maxCpuMilli ?? 0,
      });
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function remove(u: User) {
    if (!window.confirm(t('Delete user "{name}"? Their servers are NOT deleted.', { name: u.username }))) return;
    try {
      await api.deleteUser(u.id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function reset2FA(u: User) {
    if (!window.confirm(t('Reset two-factor authentication for "{name}"? They will sign in with just their password until they re-enable it.', { name: u.username }))) return;
    try {
      await api.adminDisable2FA(u.id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <div className="card">
      <h2>{t("Users")}</h2>
      <table>
        <thead>
          <tr><th>{t("User")}</th><th>{t("Role")}</th><th>{t("2FA")}</th><th>{t("Quota (servers / mem MB)")}</th><th></th></tr>
        </thead>
        <tbody>
          {users.map((u) => (
            <tr key={u.id}>
              <td>{u.username}</td>
              <td>
                {roleLabel(u)}
                {/* Superadmins assign scoped admin roles to non-superadmin users. */}
                {me.isAdmin && !u.isAdmin && (
                  <>
                    {" "}
                    <select
                      value={u.adminRoleId ?? ""}
                      onChange={(e) => setAdminRole(u, e.target.value ? Number(e.target.value) : null)}
                      style={{ width: "auto" }}
                    >
                      <option value="">{t("user (no admin)")}</option>
                      {roles.map((r) => (
                        <option key={r.id} value={r.id}>{r.name}</option>
                      ))}
                    </select>
                  </>
                )}
              </td>
              <td>{u.twoFactorEnabled ? t("on") : <span className="muted">{t("off")}</span>}</td>
              <td>{(u.maxServers || "∞") + " / " + (u.maxMemoryMB || "∞")}</td>
              <td style={{ whiteSpace: "nowrap" }}>
                {me.isAdmin && (
                  <><button onClick={() => toggleAdmin(u)}>{u.isAdmin ? t("Demote") : t("Make admin")}</button>{" "}</>
                )}
                {u.twoFactorEnabled && <><button onClick={() => reset2FA(u)}>{t("Reset 2FA")}</button>{" "}</>}
                <button className="danger" onClick={() => remove(u)}>{t("Delete")}</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      <form onSubmit={add} style={{ marginTop: 12 }}>
        <h3>{t("New user")}</h3>
        <div className="grid2">
          <div><label>{t("Username")}</label><input value={username} onChange={(e) => setUsername(e.target.value)} required /></div>
          <div><label>{t("Password")}</label><input type="password" autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} required /></div>
        </div>
        <div><label>{t("Email (optional, for password reset)")}</label><input type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="user@example.com" /></div>
        <div className="grid2">
          <div><label>{t("Max servers (0 = ∞)")}</label><input type="number" min={0} value={maxServers} onChange={(e) => setMaxServers(Number(e.target.value))} /></div>
          <div><label>{t("Max memory MB (0 = ∞)")}</label><input type="number" min={0} value={maxMemoryMB} onChange={(e) => setMaxMemoryMB(Number(e.target.value))} /></div>
        </div>
        {me.isAdmin && (
          <label className="row"><input type="checkbox" style={{ width: "auto" }} checked={isAdmin} onChange={(e) => setIsAdmin(e.target.checked)} />&nbsp;{t("Administrator (superadmin)")}</label>
        )}
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={busy || !username || !password}>
          {busy ? t("Creating…") : t("Create user")}
        </button>
      </form>
    </div>
  );
}

// Roles manages named bundles of admin permissions (superadmin only). Assigning
// a role to a user happens in the Users card.
function Roles() {
  const { t } = useT();
  const [roles, setRoles] = useState<AdminRole[]>([]);
  const [catalog, setCatalog] = useState<AdminPermInfo[]>([]);
  const [error, setError] = useState("");
  const [editing, setEditing] = useState<number | null>(null); // role id, or null for the create form
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [perms, setPerms] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);

  async function load() {
    try {
      setRoles(await api.adminRoles());
      setCatalog(await api.adminPermissions());
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, []);

  function resetForm() {
    setEditing(null);
    setName("");
    setDescription("");
    setPerms(new Set());
  }

  function startEdit(r: AdminRole) {
    setEditing(r.id);
    setName(r.name);
    setDescription(r.description);
    setPerms(new Set(r.permissions));
  }

  function togglePerm(p: string) {
    setPerms((prev) => {
      const n = new Set(prev);
      if (n.has(p)) n.delete(p);
      else n.add(p);
      return n;
    });
  }

  async function save(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    const body = { name: name.trim(), description: description.trim(), permissions: [...perms] };
    try {
      if (editing != null) await api.updateAdminRole(editing, body);
      else await api.createAdminRole(body);
      resetForm();
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function remove(r: AdminRole) {
    if (!window.confirm(t('Delete role "{name}"?', { name: r.name }))) return;
    setError("");
    try {
      await api.deleteAdminRole(r.id);
      if (editing === r.id) resetForm();
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <div className="card">
      <h2>{t("Admin roles")}</h2>
      <p className="muted">
        {t("Bundles of admin permissions you can assign to users for scoped admin access. Assign a role to a user in the Users card above.")}
      </p>
      <table>
        <thead>
          <tr><th>{t("Name")}</th><th>{t("Permissions")}</th><th></th></tr>
        </thead>
        <tbody>
          {roles.map((r) => (
            <tr key={r.id}>
              <td>{r.name}{r.description && <div className="muted" style={{ fontSize: 12 }}>{r.description}</div>}</td>
              <td>{r.permissions.length ? r.permissions.join(", ") : <span className="muted">{t("none")}</span>}</td>
              <td style={{ whiteSpace: "nowrap" }}>
                <button onClick={() => startEdit(r)}>{t("Edit")}</button>{" "}
                <button className="danger" onClick={() => remove(r)}>{t("Delete")}</button>
              </td>
            </tr>
          ))}
          {roles.length === 0 && <tr><td colSpan={3} className="muted">{t("No roles yet.")}</td></tr>}
        </tbody>
      </table>

      <form onSubmit={save} style={{ marginTop: 12 }}>
        <h3>{editing != null ? t("Edit role") : t("New role")}</h3>
        <div className="grid2">
          <div><label>{t("Name")}</label><input value={name} onChange={(e) => setName(e.target.value)} required /></div>
          <div><label>{t("Description")}</label><input value={description} onChange={(e) => setDescription(e.target.value)} /></div>
        </div>
        <label>{t("Permissions")}</label>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 4, marginTop: 4 }}>
          {catalog.map((p) => (
            <label key={p.key} className="row" title={p.description}>
              <input type="checkbox" style={{ width: "auto" }} checked={perms.has(p.key)} onChange={() => togglePerm(p.key)} />
              &nbsp;{p.key}
            </label>
          ))}
        </div>
        {error && <div className="error">{error}</div>}
        <div className="row" style={{ marginTop: 12 }}>
          <button className="primary" disabled={busy || !name.trim()}>
            {busy ? t("Saving…") : editing != null ? t("Save role") : t("Create role")}
          </button>
          {editing != null && <button type="button" onClick={resetForm}>{t("Cancel")}</button>}
        </div>
      </form>
    </div>
  );
}

function NetworkSettingsCard() {
  const { t } = useT();
  const [settings, setSettings] = useState<NetworkSettings>({ endpointHost: "", nodeAddress: "" });
  const [host, setHost] = useState("");
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function load() {
    try {
      const s = await api.networkSettings();
      setSettings(s);
      setHost(s.endpointHost || "");
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, []);

  async function save(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setMsg("");
    setError("");
    try {
      await api.setNetworkSettings(host.trim());
      setMsg(t("Saved. New endpoints use it on the next reconcile."));
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="card">
      <h2>{t("Network")}</h2>
      <p className="muted">
        {t("Public hostname shown to players in server endpoints and the SFTP connection, instead of the raw node IP. Point a DNS record at your node, then enter it here.")}
      </p>
      <form onSubmit={save}>
        <label>{t("Endpoint hostname (blank = use node IP)")}</label>
        <input value={host} onChange={(e) => setHost(e.target.value)} placeholder="play.example.com" />
        {settings.nodeAddress && (
          <p className="muted" style={{ marginTop: 4 }}>
            {t("Detected node address:")} <code>{settings.nodeAddress}</code> — {t("your DNS record should point here.")}
          </p>
        )}
        {msg && <div className="notice">{msg}</div>}
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={busy}>{busy ? t("Saving…") : t("Save")}</button>
      </form>
    </div>
  );
}

function EmailSettingsCard() {
  const { t } = useT();
  const empty: EmailSettingsInput = {
    host: "", port: "", username: "", password: "", from: "", tls: "starttls", publicUrl: "",
  };
  const [form, setForm] = useState<EmailSettingsInput>(empty);
  const [hasPassword, setHasPassword] = useState(false);
  const [configured, setConfigured] = useState(false);
  const [testTo, setTestTo] = useState("");
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function load() {
    try {
      const s = await api.emailSettings();
      setConfigured(s.configured);
      setHasPassword(s.hasPassword);
      setForm({
        host: s.host || "", port: s.port || "", username: s.username || "", password: "",
        from: s.from || "", tls: s.tls || "starttls", publicUrl: s.publicUrl || "",
      });
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, []);

  const set = (k: keyof EmailSettingsInput) => (e: { target: { value: string } }) =>
    setForm({ ...form, [k]: e.target.value });

  async function save(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setMsg("");
    setError("");
    try {
      await api.setEmailSettings(form);
      setMsg(t("Saved."));
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function test() {
    setMsg("");
    setError("");
    try {
      await api.testEmail(testTo.trim() || undefined);
      setMsg(t("Test email sent."));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  return (
    <div className="card">
      <h2>{t("Email (SMTP)")}</h2>
      <p className="muted">
        {t("Outbound email for self-service password reset.")}{" "}
        {configured ? t("Currently configured.") : t("Not configured — password reset is disabled.")}
      </p>
      <form onSubmit={save}>
        <div className="grid2">
          <div><label>{t("SMTP host (blank = disable)")}</label><input value={form.host} onChange={set("host")} placeholder="smtp.example.com" /></div>
          <div><label>{t("Port")}</label><input value={form.port} onChange={set("port")} placeholder="587" /></div>
        </div>
        <div className="grid2">
          <div><label>{t("Username")}</label><input value={form.username} onChange={set("username")} autoComplete="off" /></div>
          <div>
            <label>{t("Password")}</label>
            <input type="password" value={form.password} onChange={set("password")} autoComplete="new-password"
              placeholder={hasPassword ? t("•••••• (leave blank to keep)") : ""} />
          </div>
        </div>
        <div className="grid2">
          <div><label>{t("From address")}</label><input value={form.from} onChange={set("from")} placeholder="quetzal@example.com" /></div>
          <div>
            <label>{t("TLS")}</label>
            <select value={form.tls} onChange={set("tls")}>
              <option value="starttls">STARTTLS</option>
              <option value="tls">{t("Implicit TLS")}</option>
              <option value="none">{t("None")}</option>
            </select>
          </div>
        </div>
        <div><label>{t("Panel public URL (for reset links)")}</label><input value={form.publicUrl} onChange={set("publicUrl")} placeholder="https://quetzal.example.com" /></div>
        {msg && <div className="notice">{msg}</div>}
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={busy}>{busy ? t("Saving…") : t("Save")}</button>
      </form>
      <div className="row" style={{ marginTop: 12 }}>
        <input value={testTo} onChange={(e) => setTestTo(e.target.value)} placeholder={t("test recipient (or your email)")} style={{ flex: 1 }} />
        <button type="button" onClick={test} disabled={!configured}>{t("Send test email")}</button>
      </div>
    </div>
  );
}

function GlobalAudit() {
  const { t } = useT();
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  useEffect(() => {
    api.globalAudit().then(setEntries).catch(() => {});
  }, []);
  return (
    <div className="card">
      <Collapsible title={t("Activity log")} count={entries.length}>
        {entries.length === 0 ? (
          <p className="muted">{t("No activity yet.")}</p>
        ) : (
          <table>
            <thead><tr><th>{t("When")}</th><th>{t("User")}</th><th>{t("Server")}</th><th>{t("Action")}</th><th>{t("Detail")}</th></tr></thead>
            <tbody>
              {entries.map((e) => (
                <tr key={e.id}>
                  <td>{new Date(e.createdAt).toLocaleString()}</td>
                  <td>{e.username}</td>
                  <td>{e.serverName || (e.serverId ? `#${e.serverId}` : "—")}</td>
                  <td><code>{e.action}</code></td>
                  <td>{e.detail}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Collapsible>
    </div>
  );
}
