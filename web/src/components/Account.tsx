import { FormEvent, useEffect, useState } from "react";
import { api, APIKey, ApiError, SSHKey, User } from "../api";

export function Account({ user }: { user: User }) {
  return (
    <>
      <ChangePassword />
      <EmailCard initial={user.email || ""} />
      <TwoFactor initialEnabled={!!user.twoFactorEnabled} username={user.username} />
      <SSHKeys />
      <APIKeys />
      <div className="card">
        <h3>Account</h3>
        <div className="kv"><span className="k">Username</span><span>{user.username}</span></div>
        <div className="kv"><span className="k">Role</span><span>{user.isAdmin ? "administrator" : user.adminPerms?.length ? `scoped admin (${user.adminPerms.join(", ")})` : "user"}</span></div>
      </div>
    </>
  );
}

function TwoFactor({ initialEnabled, username }: { initialEnabled: boolean; username: string }) {
  const [enabled, setEnabled] = useState(initialEnabled);
  const [enroll, setEnroll] = useState<{ secret: string; uri: string } | null>(null);
  const [recovery, setRecovery] = useState<string[] | null>(null);
  const [disabling, setDisabling] = useState(false);
  const [code, setCode] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  // The user prop is captured at login and can be stale if 2FA was toggled
  // earlier this session; sync the real state on mount.
  useEffect(() => {
    api.me().then((m) => setEnabled(!!m.twoFactorEnabled)).catch(() => {});
  }, []);

  function fail(e: unknown) {
    setError(e instanceof ApiError ? e.message : String(e));
  }

  async function begin() {
    setError("");
    setBusy(true);
    try {
      setEnroll(await api.setup2FA());
    } catch (e) {
      fail(e);
    } finally {
      setBusy(false);
    }
  }

  async function confirm() {
    setError("");
    setBusy(true);
    try {
      const res = await api.enable2FA(code.trim());
      setRecovery(res.recoveryCodes);
      setEnabled(true);
      setEnroll(null);
      setCode("");
    } catch (e) {
      fail(e);
    } finally {
      setBusy(false);
    }
  }

  async function disable() {
    setError("");
    setBusy(true);
    try {
      await api.disable2FA(code.trim());
      setEnabled(false);
      setDisabling(false);
      setCode("");
    } catch (e) {
      fail(e);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="card">
      <h2>Two-factor authentication</h2>

      {recovery && (
        <div className="notice">
          <strong>Save your recovery codes now — they are shown only once.</strong>
          <p className="muted">Each code works once if you lose your authenticator.</p>
          <pre style={{ whiteSpace: "pre-wrap", wordBreak: "break-all" }}>{recovery.join("\n")}</pre>
          <button onClick={() => setRecovery(null)}>I've saved them</button>
        </div>
      )}

      {!recovery && enabled && !disabling && (
        <>
          <p>2FA is <strong>enabled</strong> on your account.</p>
          <button className="danger" onClick={() => { setError(""); setDisabling(true); }}>Disable 2FA</button>
        </>
      )}

      {!recovery && enabled && disabling && (
        <div>
          <p className="muted">Confirm with a current code (or a recovery code) to disable.</p>
          <label>Code</label>
          <input value={code} autoComplete="one-time-code" onChange={(e) => setCode(e.target.value)} />
          <div className="row" style={{ marginTop: 12 }}>
            <button className="danger" disabled={busy || !code} onClick={disable}>Confirm disable</button>
            <button onClick={() => { setDisabling(false); setCode(""); setError(""); }}>Cancel</button>
          </div>
        </div>
      )}

      {!recovery && !enabled && !enroll && (
        <>
          <p className="muted">Protect your account with a time-based one-time password (TOTP).</p>
          <button className="primary" disabled={busy} onClick={begin}>Enable 2FA</button>
        </>
      )}

      {!recovery && !enabled && enroll && (
        <div>
          <p className="muted">
            Add this account to your authenticator app, then enter the current code to confirm.
          </p>
          <div className="kv"><span className="k">Account</span><span>{username}</span></div>
          <label>Setup key (manual entry)</label>
          <code style={{ display: "block", wordBreak: "break-all", marginBottom: 8 }}>{enroll.secret}</code>
          <label>otpauth URI (scan or paste)</label>
          <code style={{ display: "block", wordBreak: "break-all" }}>{enroll.uri}</code>
          <label style={{ marginTop: 12 }}>Verification code</label>
          <input value={code} autoComplete="one-time-code" onChange={(e) => setCode(e.target.value)} />
          <div className="row" style={{ marginTop: 12 }}>
            <button className="primary" disabled={busy || !code} onClick={confirm}>Confirm &amp; enable</button>
            <button onClick={() => { setEnroll(null); setCode(""); setError(""); }}>Cancel</button>
          </div>
        </div>
      )}

      {error && <div className="error">{error}</div>}
    </div>
  );
}

function EmailCard({ initial }: { initial: string }) {
  const [email, setEmail] = useState(initial);
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");

  // The user prop is captured at login and can be stale; sync on mount.
  useEffect(() => {
    api.me().then((m) => setEmail(m.email || "")).catch(() => {});
  }, []);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setMsg("");
    setError("");
    try {
      const u = await api.setMyEmail(email.trim());
      setEmail(u.email || "");
      setMsg("Email saved.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  return (
    <div className="card">
      <h2>Email</h2>
      <p className="muted">Used for self-service password reset. Optional and not verified.</p>
      <form onSubmit={submit}>
        <label>Email address</label>
        <input
          type="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          placeholder="you@example.com"
        />
        {msg && <div className="notice">{msg}</div>}
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }}>Save email</button>
      </form>
    </div>
  );
}

function ChangePassword() {
  const [oldPassword, setOld] = useState("");
  const [newPassword, setNew] = useState("");
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");

  async function submit(e: FormEvent) {
    e.preventDefault();
    setMsg("");
    setError("");
    try {
      await api.changePassword(oldPassword, newPassword);
      setOld("");
      setNew("");
      setMsg("Password changed.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  return (
    <div className="card">
      <h2>Change password</h2>
      <form onSubmit={submit}>
        <label>Current password</label>
        <input type="password" autoComplete="current-password" value={oldPassword} onChange={(e) => setOld(e.target.value)} required />
        <label>New password</label>
        <input type="password" autoComplete="new-password" value={newPassword} onChange={(e) => setNew(e.target.value)} required />
        {msg && <div className="notice">{msg}</div>}
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={!oldPassword || !newPassword}>Update password</button>
      </form>
    </div>
  );
}

function SSHKeys() {
  const [keys, setKeys] = useState<SSHKey[]>([]);
  const [name, setName] = useState("");
  const [pub, setPub] = useState("");
  const [error, setError] = useState("");

  async function load() {
    try {
      setKeys(await api.sshKeys());
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, []);

  async function add(e: FormEvent) {
    e.preventDefault();
    setError("");
    try {
      await api.addSSHKey(name.trim(), pub.trim());
      setName("");
      setPub("");
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  async function remove(k: SSHKey) {
    if (!window.confirm(`Delete SSH key "${k.name}"?`)) return;
    await api.deleteSSHKey(k.id).catch((e) => setError(String(e)));
    await load();
  }

  return (
    <div className="card">
      <h2>SSH keys</h2>
      <p className="muted">
        Public keys authorized for SFTP access to servers you can manage files on.
      </p>
      {keys.length === 0 ? (
        <p className="muted">No SSH keys.</p>
      ) : (
        <table>
          <thead><tr><th>Name</th><th>Fingerprint</th><th></th></tr></thead>
          <tbody>
            {keys.map((k) => (
              <tr key={k.id}>
                <td>{k.name}</td>
                <td><code style={{ fontSize: 12 }}>{k.fingerprint}</code></td>
                <td><button className="danger" onClick={() => remove(k)}>Delete</button></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <form onSubmit={add} style={{ marginTop: 12 }}>
        <h3>Add a key</h3>
        <label>Name (optional)</label>
        <input value={name} placeholder="laptop" onChange={(e) => setName(e.target.value)} />
        <label>Public key</label>
        <textarea
          value={pub}
          onChange={(e) => setPub(e.target.value)}
          placeholder="ssh-ed25519 AAAA… you@host"
          spellCheck={false}
          style={{ width: "100%", minHeight: 70, fontFamily: "monospace" }}
        />
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 8 }} disabled={!pub.trim()}>Add key</button>
      </form>
    </div>
  );
}

function APIKeys() {
  const [keys, setKeys] = useState<APIKey[]>([]);
  const [name, setName] = useState("");
  const [fresh, setFresh] = useState("");
  const [error, setError] = useState("");

  async function load() {
    try {
      setKeys(await api.apiKeys());
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, []);

  async function create(e: FormEvent) {
    e.preventDefault();
    setError("");
    try {
      const res = await api.createAPIKey(name);
      setFresh(res.token);
      setName("");
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  async function remove(k: APIKey) {
    if (!window.confirm(`Revoke API key "${k.name}"?`)) return;
    await api.deleteAPIKey(k.id).catch((e) => setError(String(e)));
    await load();
  }

  return (
    <div className="card">
      <h2>API keys</h2>
      <p className="muted">
        Use as a bearer token: <code>Authorization: Bearer &lt;token&gt;</code>. A key inherits your permissions.
      </p>
      {fresh && (
        <div className="notice">
          New token (shown once — copy it now): <code style={{ wordBreak: "break-all" }}>{fresh}</code>
        </div>
      )}
      {keys.length === 0 ? (
        <p className="muted">No API keys.</p>
      ) : (
        <table>
          <thead><tr><th>Name</th><th>Prefix</th><th>Last used</th><th></th></tr></thead>
          <tbody>
            {keys.map((k) => (
              <tr key={k.id}>
                <td>{k.name}</td>
                <td><code>{k.prefix}…</code></td>
                <td>{k.lastUsedAt ? new Date(k.lastUsedAt).toLocaleString() : "never"}</td>
                <td><button className="danger" onClick={() => remove(k)}>Revoke</button></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <form onSubmit={create} className="row" style={{ marginTop: 12 }}>
        <input value={name} placeholder="key name (e.g. ci)" onChange={(e) => setName(e.target.value)} required />
        <button className="primary" disabled={!name}>Create key</button>
      </form>
      {error && <div className="error">{error}</div>}
    </div>
  );
}
