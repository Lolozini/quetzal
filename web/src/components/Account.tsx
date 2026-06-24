import { FormEvent, useEffect, useState } from "react";
import { api, APIKey, ApiError, SSHKey, User } from "../api";
import { useT } from "../i18n";

export function Account({ user }: { user: User }) {
  const { t } = useT();
  return (
    <>
      <ChangePassword />
      <EmailCard initial={user.email || ""} />
      <TwoFactor initialEnabled={!!user.twoFactorEnabled} username={user.username} />
      <SSHKeys />
      <APIKeys />
      <div className="card">
        <h3>{t("Account")}</h3>
        <div className="kv"><span className="k">{t("Username")}</span><span>{user.username}</span></div>
        <div className="kv"><span className="k">{t("Role")}</span><span>{user.isAdmin ? t("administrator") : user.adminPerms?.length ? t("scoped admin ({perms})", { perms: user.adminPerms.join(", ") }) : t("user")}</span></div>
      </div>
    </>
  );
}

function TwoFactor({ initialEnabled, username }: { initialEnabled: boolean; username: string }) {
  const { t } = useT();
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
      <h2>{t("Two-factor authentication")}</h2>

      {recovery && (
        <div className="notice">
          <strong>{t("Save your recovery codes now — they are shown only once.")}</strong>
          <p className="muted">{t("Each code works once if you lose your authenticator.")}</p>
          <pre style={{ whiteSpace: "pre-wrap", wordBreak: "break-all" }}>{recovery.join("\n")}</pre>
          <button onClick={() => setRecovery(null)}>{t("I've saved them")}</button>
        </div>
      )}

      {!recovery && enabled && !disabling && (
        <>
          <p>{t("2FA is enabled on your account.")}</p>
          <button className="danger" onClick={() => { setError(""); setDisabling(true); }}>{t("Disable 2FA")}</button>
        </>
      )}

      {!recovery && enabled && disabling && (
        <div>
          <p className="muted">{t("Confirm with a current code (or a recovery code) to disable.")}</p>
          <label>{t("Code")}</label>
          <input value={code} autoComplete="one-time-code" onChange={(e) => setCode(e.target.value)} />
          <div className="row" style={{ marginTop: 12 }}>
            <button className="danger" disabled={busy || !code} onClick={disable}>{t("Confirm disable")}</button>
            <button onClick={() => { setDisabling(false); setCode(""); setError(""); }}>{t("Cancel")}</button>
          </div>
        </div>
      )}

      {!recovery && !enabled && !enroll && (
        <>
          <p className="muted">{t("Protect your account with a time-based one-time password (TOTP).")}</p>
          <button className="primary" disabled={busy} onClick={begin}>{t("Enable 2FA")}</button>
        </>
      )}

      {!recovery && !enabled && enroll && (
        <div>
          <p className="muted">
            {t("Add this account to your authenticator app, then enter the current code to confirm.")}
          </p>
          <div className="kv"><span className="k">{t("Account")}</span><span>{username}</span></div>
          <label>{t("Setup key (manual entry)")}</label>
          <code style={{ display: "block", wordBreak: "break-all", marginBottom: 8 }}>{enroll.secret}</code>
          <label>{t("otpauth URI (scan or paste)")}</label>
          <code style={{ display: "block", wordBreak: "break-all" }}>{enroll.uri}</code>
          <label style={{ marginTop: 12 }}>{t("Verification code")}</label>
          <input value={code} autoComplete="one-time-code" onChange={(e) => setCode(e.target.value)} />
          <div className="row" style={{ marginTop: 12 }}>
            <button className="primary" disabled={busy || !code} onClick={confirm}>{t("Confirm & enable")}</button>
            <button onClick={() => { setEnroll(null); setCode(""); setError(""); }}>{t("Cancel")}</button>
          </div>
        </div>
      )}

      {error && <div className="error">{error}</div>}
    </div>
  );
}

function EmailCard({ initial }: { initial: string }) {
  const { t } = useT();
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
      setMsg(t("Email saved."));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  return (
    <div className="card">
      <h2>{t("Email")}</h2>
      <p className="muted">{t("Used for self-service password reset. Optional and not verified.")}</p>
      <form onSubmit={submit}>
        <label>{t("Email address")}</label>
        <input
          type="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          placeholder="you@example.com"
        />
        {msg && <div className="notice">{msg}</div>}
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }}>{t("Save email")}</button>
      </form>
    </div>
  );
}

function ChangePassword() {
  const { t } = useT();
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
      setMsg(t("Password changed."));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  return (
    <div className="card">
      <h2>{t("Change password")}</h2>
      <form onSubmit={submit}>
        <label>{t("Current password")}</label>
        <input type="password" autoComplete="current-password" value={oldPassword} onChange={(e) => setOld(e.target.value)} required />
        <label>{t("New password")}</label>
        <input type="password" autoComplete="new-password" value={newPassword} onChange={(e) => setNew(e.target.value)} required />
        {msg && <div className="notice">{msg}</div>}
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={!oldPassword || !newPassword}>{t("Update password")}</button>
      </form>
    </div>
  );
}

function SSHKeys() {
  const { t } = useT();
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
    if (!window.confirm(t('Delete SSH key "{name}"?', { name: k.name }))) return;
    await api.deleteSSHKey(k.id).catch((e) => setError(String(e)));
    await load();
  }

  return (
    <div className="card">
      <h2>{t("SSH keys")}</h2>
      <p className="muted">
        {t("Public keys authorized for SFTP access to servers you can manage files on.")}
      </p>
      {keys.length === 0 ? (
        <p className="muted">{t("No SSH keys.")}</p>
      ) : (
        <table>
          <thead><tr><th>{t("Name")}</th><th>{t("Fingerprint")}</th><th></th></tr></thead>
          <tbody>
            {keys.map((k) => (
              <tr key={k.id}>
                <td>{k.name}</td>
                <td><code style={{ fontSize: 12 }}>{k.fingerprint}</code></td>
                <td><button className="danger" onClick={() => remove(k)}>{t("Delete")}</button></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <form onSubmit={add} style={{ marginTop: 12 }}>
        <h3>{t("Add a key")}</h3>
        <label>{t("Name (optional)")}</label>
        <input value={name} placeholder={t("laptop")} onChange={(e) => setName(e.target.value)} />
        <label>{t("Public key")}</label>
        <textarea
          value={pub}
          onChange={(e) => setPub(e.target.value)}
          placeholder="ssh-ed25519 AAAA… you@host"
          spellCheck={false}
          style={{ width: "100%", minHeight: 70, fontFamily: "monospace" }}
        />
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 8 }} disabled={!pub.trim()}>{t("Add key")}</button>
      </form>
    </div>
  );
}

function APIKeys() {
  const { t } = useT();
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
    if (!window.confirm(t('Revoke API key "{name}"?', { name: k.name }))) return;
    await api.deleteAPIKey(k.id).catch((e) => setError(String(e)));
    await load();
  }

  return (
    <div className="card">
      <h2>{t("API keys")}</h2>
      <p className="muted">
        {t("Use as a bearer token:")} <code>Authorization: Bearer &lt;token&gt;</code>. {t("A key inherits your permissions.")}
      </p>
      {fresh && (
        <div className="notice">
          {t("New token (shown once — copy it now):")} <code style={{ wordBreak: "break-all" }}>{fresh}</code>
        </div>
      )}
      {keys.length === 0 ? (
        <p className="muted">{t("No API keys.")}</p>
      ) : (
        <table>
          <thead><tr><th>{t("Name")}</th><th>{t("Prefix")}</th><th>{t("Last used")}</th><th></th></tr></thead>
          <tbody>
            {keys.map((k) => (
              <tr key={k.id}>
                <td>{k.name}</td>
                <td><code>{k.prefix}…</code></td>
                <td>{k.lastUsedAt ? new Date(k.lastUsedAt).toLocaleString() : t("never")}</td>
                <td><button className="danger" onClick={() => remove(k)}>{t("Revoke")}</button></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <form onSubmit={create} className="row" style={{ marginTop: 12 }}>
        <input value={name} placeholder={t("key name (e.g. ci)")} onChange={(e) => setName(e.target.value)} required />
        <button className="primary" disabled={!name}>{t("Create key")}</button>
      </form>
      {error && <div className="error">{error}</div>}
    </div>
  );
}
