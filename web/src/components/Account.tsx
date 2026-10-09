import { FormEvent, useEffect, useId, useRef, useState } from "react";
import { api, APIKey, ApiError, SSHKey, User, errorMessage } from "../api";
import { useT } from "../i18n";
import { QRCode } from "./QRCode";

export function Account({ user, onUserRefresh }: { user: User; onUserRefresh: () => Promise<void> }) {
  const { t } = useT();
  return (
    <>
      <ChangePassword />
      <EmailCard initial={user.email || ""} />
      <TwoFactor initialEnabled={!!user.twoFactorEnabled} username={user.username} onChanged={onUserRefresh} />
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

// Exported so the enrolment wall can reuse it: when the panel requires a second
// factor, an account without one sees this and nothing else.
export function TwoFactor({
  initialEnabled,
  username,
  onChanged,
}: {
  initialEnabled: boolean;
  username: string;
  onChanged: () => Promise<void>;
}) {
  const fieldId = useId();
  const { t } = useT();
  const [enabled, setEnabled] = useState(initialEnabled);
  const [enroll, setEnroll] = useState<{ secret: string; uri: string } | null>(null);
  const [recovery, setRecovery] = useState<string[] | null>(null);
  const [disabling, setDisabling] = useState(false);
  const [refreshPending, setRefreshPending] = useState(false);
  const [code, setCode] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  // Read the current state on mount, including changes made in another tab.
  useEffect(() => {
    api.me().then((m) => setEnabled(!!m.twoFactorEnabled)).catch(() => {});
  }, []);

  function fail(e: unknown) {
    setError(errorMessage(e, t));
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

  async function refresh() {
    setError("");
    setBusy(true);
    try {
      // Do not release the enrolment wall until the codes were acknowledged.
      // If refresh fails, keep them visible and let the same button retry.
      await onChanged();
      setRecovery(null);
      setRefreshPending(false);
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
      setRefreshPending(true);
      await onChanged();
      setRefreshPending(false);
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
          <button disabled={busy} onClick={refresh}>{t("I've saved them")}</button>
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
          <p id={`${fieldId}-disable-help`} className="muted">{t("Confirm with a current code (or a recovery code) to disable.")}</p>
          <label htmlFor={`${fieldId}-disable-code`}>{t("Code")}</label>
          <input aria-describedby={`${fieldId}-disable-help`} id={`${fieldId}-disable-code`} value={code} autoComplete="one-time-code" onChange={(e) => setCode(e.target.value)} />
          <div className="row" style={{ marginTop: 12 }}>
            <button className="danger" disabled={busy || !code} onClick={disable}>{t("Confirm disable")}</button>
            <button onClick={() => { setDisabling(false); setCode(""); setError(""); }}>{t("Cancel")}</button>
          </div>
        </div>
      )}

      {!recovery && !enabled && !enroll && (
        <>
          <p className="muted">{t("Protect your account with a time-based one-time password (TOTP).")}</p>
          <button className="primary" disabled={busy || refreshPending} onClick={begin}>{t("Enable 2FA")}</button>
        </>
      )}

      {!recovery && !enabled && enroll && (
        <div>
          <p id={`${fieldId}-verification-help`} className="muted">
            {t("Scan this QR code with your authenticator app, or enter the setup key by hand, then type the code it shows to confirm.")}
          </p>
          <QRCode value={enroll.uri} label={t("QR code that adds this account to an authenticator app")} />
          <div className="kv"><span className="k">{t("Account")}</span><span>{username}</span></div>
          <div style={{ display: "block", margin: "10px 0 4px", color: "var(--ink-muted)", fontSize: 13 }}>{t("Setup key (manual entry)")}</div>
          <code style={{ display: "block", wordBreak: "break-all", marginBottom: 8 }}>{enroll.secret}</code>
          <div style={{ display: "block", margin: "10px 0 4px", color: "var(--ink-muted)", fontSize: 13 }}>{t("otpauth URI (to paste)")}</div>
          <code style={{ display: "block", wordBreak: "break-all" }}>{enroll.uri}</code>
          <label htmlFor={`${fieldId}-verification-code`} style={{ marginTop: 12 }}>{t("Verification code")}</label>
          <input aria-describedby={`${fieldId}-verification-help`} id={`${fieldId}-verification-code`} value={code} autoComplete="one-time-code" onChange={(e) => setCode(e.target.value)} />
          <div className="row" style={{ marginTop: 12 }}>
            <button className="primary" disabled={busy || !code} onClick={confirm}>{t("Confirm & enable")}</button>
            <button onClick={() => { setEnroll(null); setCode(""); setError(""); }}>{t("Cancel")}</button>
          </div>
        </div>
      )}

      {refreshPending && <button disabled={busy} onClick={refresh}>{t("Refresh")}</button>}
      {error && <div className="error">{error}</div>}
    </div>
  );
}

function EmailCard({ initial }: { initial: string }) {
  const fieldId = useId();
  const { t } = useT();
  const [email, setEmail] = useState(initial);
  const [me, setMe] = useState<User | null>(null);
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  function take(u: User) {
    setMe(u);
    setEmail(u.email || "");
  }

  // The user prop is captured at login and can be stale; sync on mount.
  useEffect(() => {
    api.me().then(take).catch(() => {});
  }, []);

  async function run(action: () => Promise<User>, done: (u: User) => string) {
    setMsg("");
    setError("");
    setBusy(true);
    try {
      const u = await action();
      take(u);
      setMsg(done(u));
    } catch (err) {
      setError(errorMessage(err, t));
    } finally {
      setBusy(false);
    }
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    const wanted = email.trim();
    run(
      () => api.setMyEmail(wanted),
      (u) =>
        u.pendingEmail
          ? t("We sent a link to {address}. Your address changes once you open it.", { address: u.pendingEmail })
          : t("Email saved."),
    );
  }

  const current = me?.email || "";
  return (
    <div className="card">
      <h2>{t("Email")}</h2>
      <p className="muted">{t("Used for self-service password reset. Optional.")}</p>
      {current && (
        <div className="kv">
          <span className="k">{t("Current address")}</span>
          <span>
            {current}{" "}
            {me?.emailVerified ? (
              <span className="badge ok">{t("confirmed")}</span>
            ) : (
              <span className="badge warn">{t("not confirmed")}</span>
            )}
          </span>
        </div>
      )}
      {me?.pendingEmail && (
        <div className="notice">
          {t("Waiting for confirmation: {address}. Open the link we mailed to it.", { address: me.pendingEmail })}
          <div className="row" style={{ marginTop: 8, gap: 8 }}>
            <button type="button" disabled={busy} onClick={() => run(api.resendEmailConfirmation, () => t("Link sent again."))}>
              {t("Send the link again")}
            </button>
            <button type="button" disabled={busy} onClick={() => run(api.cancelPendingEmail, () => t("Change cancelled."))}>
              {t("Cancel the change")}
            </button>
          </div>
        </div>
      )}
      {current && !me?.emailVerified && !me?.pendingEmail && (
        <button
          type="button"
          disabled={busy}
          onClick={() => run(api.resendEmailConfirmation, (u) => t("We sent a link to {address}.", { address: u.email || "" }))}
        >
          {t("Confirm this address")}
        </button>
      )}
      <form onSubmit={submit}>
        <label htmlFor={`${fieldId}-email`}>{t("Email address")}</label>
        <input id={`${fieldId}-email`}
          type="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          placeholder="you@example.com"
        />
        {msg && <div className="notice">{msg}</div>}
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={busy}>{t("Save email")}</button>
      </form>
    </div>
  );
}

function ChangePassword() {
  const fieldId = useId();
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
      setError(errorMessage(err, t));
    }
  }

  return (
    <div className="card">
      <h2>{t("Change password")}</h2>
      <form onSubmit={submit}>
        <label htmlFor={`${fieldId}-current-password`}>{t("Current password")}</label>
        <input id={`${fieldId}-current-password`} type="password" autoComplete="current-password" value={oldPassword} onChange={(e) => setOld(e.target.value)} required />
        <label htmlFor={`${fieldId}-new-password`}>{t("New password")}</label>
        <input id={`${fieldId}-new-password`} type="password" autoComplete="new-password" value={newPassword} onChange={(e) => setNew(e.target.value)} required minLength={8} aria-describedby={`${fieldId}-password-help`} />
        <p id={`${fieldId}-password-help`} className="muted">{t("Password must be at least 8 characters.")}</p>
        {msg && <div className="notice">{msg}</div>}
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={!oldPassword || !newPassword}>{t("Update password")}</button>
      </form>
    </div>
  );
}

function SSHKeys() {
  const fieldId = useId();
  const { t } = useT();
  const [keys, setKeys] = useState<SSHKey[]>([]);
  const [name, setName] = useState("");
  const [pub, setPub] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  async function load() {
    try {
      setKeys(await api.sshKeys());
    } catch (e) {
      setError(errorMessage(e, t));
    }
  }
  useEffect(() => {
    load();
  }, []);

  async function add(e: FormEvent) {
    e.preventDefault();
    setError("");
    setNotice("");
    try {
      await api.addSSHKey(name.trim(), pub.trim());
      setName("");
      setPub("");
      setNotice(t("Key added. SFTP accepts it within a minute or two, on the servers whose files you can manage."));
      await load();
    } catch (err) {
      const existing = err instanceof ApiError && err.status === 409 ? (err.data as { existing?: { name: string } })?.existing : undefined;
      if (existing) setError(t('This key is already on your account, as "{name}".', { name: existing.name }));
      else setError(errorMessage(err, t));
    }
  }

  async function remove(k: SSHKey) {
    if (!window.confirm(t('Delete SSH key "{name}"?', { name: k.name }))) return;
    setError("");
    setNotice("");
    try {
      await api.deleteSSHKey(k.id);
      setNotice(t("Key deleted. SFTP refuses it within a minute or two, and closes the sessions opened with it."));
    } catch (e) {
      setError(errorMessage(e, t));
    }
    await load();
  }

  return (
    <div className="card">
      <h2>{t("SSH keys")}</h2>
      <p className="muted">
        {t("Public keys authorized for SFTP access to servers you can manage files on.")}
      </p>
      {notice && <div className="notice">{notice}</div>}
      {keys.length === 0 ? (
        <p className="muted">{t("No SSH keys.")}</p>
      ) : (
        <div className="table-scroll">
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
        </div>
      )}
      <form onSubmit={add} style={{ marginTop: 12 }}>
        <h3>{t("Add a key")}</h3>
        <label htmlFor={`${fieldId}-key-name`}>{t("Name (optional)")}</label>
        <input id={`${fieldId}-key-name`} value={name} placeholder={t("laptop")} onChange={(e) => setName(e.target.value)} />
        <label htmlFor={`${fieldId}-public-key`}>{t("Public key")}</label>
        <textarea id={`${fieldId}-public-key`}
          value={pub}
          onChange={(e) => setPub(e.target.value)}
          placeholder="ssh-ed25519 AAAA… you@host"
          spellCheck={false}
          style={{ width: "100%", minHeight: 70, fontFamily: "var(--font-mono)" }}
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
  const [busy, setBusy] = useState(false);
  const creating = useRef(false);

  async function load() {
    try {
      setKeys(await api.apiKeys());
    } catch (e) {
      setError(errorMessage(e, t));
    }
  }
  useEffect(() => {
    load();
  }, []);

  async function create(e: FormEvent) {
    e.preventDefault();
    if (creating.current || !name) return;
    creating.current = true;
    setBusy(true);
    setError("");
    try {
      const res = await api.createAPIKey(name);
      setFresh(res.token);
      setName("");
      await load();
    } catch (err) {
      setError(errorMessage(err, t));
    } finally {
      creating.current = false;
      setBusy(false);
    }
  }

  async function remove(k: APIKey) {
    if (!window.confirm(t('Revoke API key "{name}"?', { name: k.name }))) return;
    await api.deleteAPIKey(k.id).catch((e) => setError(errorMessage(e, t)));
    await load();
  }

  return (
    <div className="card">
      <h2>{t("API keys")}</h2>
      <p className="muted">
        {t("Use as a bearer token:")} <code>Authorization: Bearer &lt;token&gt;</code>. {t("A key inherits your permissions.")}{" "}
        {t("It signs in without two-factor authentication: keep it as safe as your password and your second factor together.")}
      </p>
      {fresh && (
        <div className="notice">
          {t("New token (shown once — copy it now):")} <code style={{ wordBreak: "break-all" }}>{fresh}</code>
        </div>
      )}
      {keys.length === 0 ? (
        <p className="muted">{t("No API keys.")}</p>
      ) : (
        <div className="table-scroll">
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
        </div>
      )}
      <form onSubmit={create} className="row" style={{ marginTop: 12 }} aria-busy={busy}>
        <input aria-label={t("key name (e.g. ci)")} value={name} placeholder={t("key name (e.g. ci)")} onChange={(e) => setName(e.target.value)} required disabled={busy} />
        <button className="primary" disabled={busy || !name}>{busy ? t("Creating…") : t("Create key")}</button>
      </form>
      {error && <div className="error">{error}</div>}
    </div>
  );
}
