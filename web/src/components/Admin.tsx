import { FormEvent, useEffect, useState } from "react";
import { AdminPermInfo, AdminRole, api, ApiError, AuditEntry, EmailSettingsInput, hasAdminPerm, User } from "../api";
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
    if (u.isAdmin) return "superadmin";
    if (u.adminRoleId != null) {
      // roles are only loaded for superadmins; fall back to a generic label.
      const r = roles.find((x) => x.id === u.adminRoleId);
      return r ? `admin: ${r.name}` : "scoped admin";
    }
    return "user";
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
    if (!window.confirm(`Delete user "${u.username}"? Their servers are NOT deleted.`)) return;
    try {
      await api.deleteUser(u.id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function reset2FA(u: User) {
    if (!window.confirm(`Reset two-factor authentication for "${u.username}"? They will sign in with just their password until they re-enable it.`)) return;
    try {
      await api.adminDisable2FA(u.id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <div className="card">
      <h2>Users</h2>
      <table>
        <thead>
          <tr><th>User</th><th>Role</th><th>2FA</th><th>Quota (servers / mem MB)</th><th></th></tr>
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
                      <option value="">user (no admin)</option>
                      {roles.map((r) => (
                        <option key={r.id} value={r.id}>{r.name}</option>
                      ))}
                    </select>
                  </>
                )}
              </td>
              <td>{u.twoFactorEnabled ? "on" : <span className="muted">off</span>}</td>
              <td>{(u.maxServers || "∞") + " / " + (u.maxMemoryMB || "∞")}</td>
              <td style={{ whiteSpace: "nowrap" }}>
                {me.isAdmin && (
                  <><button onClick={() => toggleAdmin(u)}>{u.isAdmin ? "Demote" : "Make admin"}</button>{" "}</>
                )}
                {u.twoFactorEnabled && <><button onClick={() => reset2FA(u)}>Reset 2FA</button>{" "}</>}
                <button className="danger" onClick={() => remove(u)}>Delete</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      <form onSubmit={add} style={{ marginTop: 12 }}>
        <h3>New user</h3>
        <div className="grid2">
          <div><label>Username</label><input value={username} onChange={(e) => setUsername(e.target.value)} required /></div>
          <div><label>Password</label><input type="password" autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} required /></div>
        </div>
        <div><label>Email (optional, for password reset)</label><input type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="user@example.com" /></div>
        <div className="grid2">
          <div><label>Max servers (0 = ∞)</label><input type="number" min={0} value={maxServers} onChange={(e) => setMaxServers(Number(e.target.value))} /></div>
          <div><label>Max memory MB (0 = ∞)</label><input type="number" min={0} value={maxMemoryMB} onChange={(e) => setMaxMemoryMB(Number(e.target.value))} /></div>
        </div>
        {me.isAdmin && (
          <label className="row"><input type="checkbox" style={{ width: "auto" }} checked={isAdmin} onChange={(e) => setIsAdmin(e.target.checked)} />&nbsp;Administrator (superadmin)</label>
        )}
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={busy || !username || !password}>
          {busy ? "Creating…" : "Create user"}
        </button>
      </form>
    </div>
  );
}

// Roles manages named bundles of admin permissions (superadmin only). Assigning
// a role to a user happens in the Users card.
function Roles() {
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
    if (!window.confirm(`Delete role "${r.name}"?`)) return;
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
      <h2>Admin roles</h2>
      <p className="muted">
        Bundles of admin permissions you can assign to users for scoped admin
        access. Assign a role to a user in the Users card above.
      </p>
      <table>
        <thead>
          <tr><th>Name</th><th>Permissions</th><th></th></tr>
        </thead>
        <tbody>
          {roles.map((r) => (
            <tr key={r.id}>
              <td>{r.name}{r.description && <div className="muted" style={{ fontSize: 12 }}>{r.description}</div>}</td>
              <td>{r.permissions.length ? r.permissions.join(", ") : <span className="muted">none</span>}</td>
              <td style={{ whiteSpace: "nowrap" }}>
                <button onClick={() => startEdit(r)}>Edit</button>{" "}
                <button className="danger" onClick={() => remove(r)}>Delete</button>
              </td>
            </tr>
          ))}
          {roles.length === 0 && <tr><td colSpan={3} className="muted">No roles yet.</td></tr>}
        </tbody>
      </table>

      <form onSubmit={save} style={{ marginTop: 12 }}>
        <h3>{editing != null ? "Edit role" : "New role"}</h3>
        <div className="grid2">
          <div><label>Name</label><input value={name} onChange={(e) => setName(e.target.value)} required /></div>
          <div><label>Description</label><input value={description} onChange={(e) => setDescription(e.target.value)} /></div>
        </div>
        <label>Permissions</label>
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
            {busy ? "Saving…" : editing != null ? "Save role" : "Create role"}
          </button>
          {editing != null && <button type="button" onClick={resetForm}>Cancel</button>}
        </div>
      </form>
    </div>
  );
}

function EmailSettingsCard() {
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
      setMsg("Saved.");
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
      setMsg("Test email sent.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  return (
    <div className="card">
      <h2>Email (SMTP)</h2>
      <p className="muted">
        Outbound email for self-service password reset.{" "}
        {configured ? "Currently configured." : "Not configured — password reset is disabled."}
      </p>
      <form onSubmit={save}>
        <div className="grid2">
          <div><label>SMTP host (blank = disable)</label><input value={form.host} onChange={set("host")} placeholder="smtp.example.com" /></div>
          <div><label>Port</label><input value={form.port} onChange={set("port")} placeholder="587" /></div>
        </div>
        <div className="grid2">
          <div><label>Username</label><input value={form.username} onChange={set("username")} autoComplete="off" /></div>
          <div>
            <label>Password</label>
            <input type="password" value={form.password} onChange={set("password")} autoComplete="new-password"
              placeholder={hasPassword ? "•••••• (leave blank to keep)" : ""} />
          </div>
        </div>
        <div className="grid2">
          <div><label>From address</label><input value={form.from} onChange={set("from")} placeholder="quetzal@example.com" /></div>
          <div>
            <label>TLS</label>
            <select value={form.tls} onChange={set("tls")}>
              <option value="starttls">STARTTLS</option>
              <option value="tls">Implicit TLS</option>
              <option value="none">None</option>
            </select>
          </div>
        </div>
        <div><label>Panel public URL (for reset links)</label><input value={form.publicUrl} onChange={set("publicUrl")} placeholder="https://quetzal.example.com" /></div>
        {msg && <div className="notice">{msg}</div>}
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={busy}>{busy ? "Saving…" : "Save"}</button>
      </form>
      <div className="row" style={{ marginTop: 12 }}>
        <input value={testTo} onChange={(e) => setTestTo(e.target.value)} placeholder="test recipient (or your email)" style={{ flex: 1 }} />
        <button type="button" onClick={test} disabled={!configured}>Send test email</button>
      </div>
    </div>
  );
}

function GlobalAudit() {
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  useEffect(() => {
    api.globalAudit().then(setEntries).catch(() => {});
  }, []);
  return (
    <div className="card">
      <h2>Activity log</h2>
      {entries.length === 0 ? (
        <p className="muted">No activity yet.</p>
      ) : (
        <table>
          <thead><tr><th>When</th><th>User</th><th>Action</th><th>Detail</th></tr></thead>
          <tbody>
            {entries.map((e) => (
              <tr key={e.id}>
                <td>{new Date(e.createdAt).toLocaleString()}</td>
                <td>{e.username}</td>
                <td><code>{e.action}</code></td>
                <td>{e.detail}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
