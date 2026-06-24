import { FormEvent, useEffect, useState } from "react";
import { api, ApiError, ChannelType, EVENT_TYPES, NotificationChannel } from "../api";
import { useT } from "../i18n";

type FieldDef = {
  key: string;
  label: string;
  secret?: boolean;
  select?: string[];
  placeholder?: string;
};

// Config fields per channel type. Secret fields are write-only: on edit they
// render blank with a "(configured)" hint and are omitted when left blank.
const FIELDS: Record<ChannelType, FieldDef[]> = {
  discord: [
    { key: "url", label: "Webhook URL", secret: true, placeholder: "https://discord.com/api/webhooks/…" },
  ],
  webhook: [
    { key: "url", label: "URL", secret: true, placeholder: "https://example.com/hook" },
    { key: "secret", label: "Signing secret (optional)", secret: true },
  ],
  email: [
    { key: "host", label: "SMTP host" },
    { key: "port", label: "Port", placeholder: "587" },
    { key: "from", label: "From address" },
    { key: "to", label: "To (comma-separated)" },
    { key: "username", label: "Username (optional)" },
    { key: "password", label: "Password (optional)", secret: true },
    { key: "tls", label: "TLS", select: ["starttls", "tls", "none"] },
  ],
};

const blankForm = (serverId: number) => ({
  id: 0,
  name: "",
  type: "discord" as ChannelType,
  enabled: true,
  serverId,
  events: [] as string[],
  config: {} as Record<string, string>,
  secrets: {} as Record<string, boolean>,
});

export function Notifications({ serverId }: { serverId: number }) {
  const { t } = useT();
  const [channels, setChannels] = useState<NotificationChannel[]>([]);
  const [form, setForm] = useState(blankForm(serverId));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState<Record<number, string>>({});

  async function load() {
    try {
      const all = serverId === 0 ? await api.channels() : await api.serverChannels(serverId);
      // The global admin view manages catch-all channels; server-scoped ones are
      // managed on their server page.
      setChannels(serverId === 0 ? all.filter((c) => c.serverId === 0) : all);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [serverId]);

  function reset() {
    setForm(blankForm(serverId));
  }

  function edit(c: NotificationChannel) {
    setForm({
      id: c.id,
      name: c.name,
      type: c.type,
      enabled: c.enabled,
      serverId: c.serverId,
      events: c.events ?? [],
      config: { ...c.config },
      secrets: c.secrets ?? {},
    });
  }

  function setConfig(key: string, value: string) {
    setForm((f) => ({ ...f, config: { ...f.config, [key]: value } }));
  }

  function toggleEvent(ev: string) {
    setForm((f) => ({
      ...f,
      events: f.events.includes(ev) ? f.events.filter((e) => e !== ev) : [...f.events, ev],
    }));
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    // Drop blank secret values so they keep their stored value on update.
    const cfg: Record<string, string> = {};
    for (const fld of FIELDS[form.type]) {
      const v = (form.config[fld.key] ?? "").trim();
      if (v === "" && fld.secret) continue;
      cfg[fld.key] = v;
    }
    try {
      if (form.id === 0) {
        await api.createChannel({
          name: form.name, type: form.type, enabled: form.enabled,
          serverId, events: form.events, config: cfg,
        });
      } else {
        await api.updateChannel(form.id, {
          name: form.name, enabled: form.enabled, events: form.events, config: cfg,
        });
      }
      reset();
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function toggle(c: NotificationChannel) {
    try {
      await api.updateChannel(c.id, { name: c.name, enabled: !c.enabled, events: c.events });
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function test(c: NotificationChannel) {
    setStatus((s) => ({ ...s, [c.id]: "sending…" }));
    try {
      await api.testChannel(c.id);
      setStatus((s) => ({ ...s, [c.id]: "sent ✓" }));
    } catch (e) {
      setStatus((s) => ({ ...s, [c.id]: e instanceof ApiError ? e.message : String(e) }));
    }
  }

  async function remove(c: NotificationChannel) {
    if (!window.confirm(t('Delete notification channel "{name}"?', { name: c.name }))) return;
    try {
      await api.deleteChannel(c.id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <div className="card">
      <h2>{t("Notifications")}</h2>
      <p className="muted">
        {serverId === 0
          ? t("Global channels receive every event (panel and all servers).")
          : t("Channels here receive only this server's events.")}
      </p>

      {channels.length === 0 ? (
        <p className="muted">{t("No channels yet.")}</p>
      ) : (
        <table>
          <thead>
            <tr><th>{t("Name")}</th><th>{t("Type")}</th><th>{t("Events")}</th><th>{t("Status")}</th><th></th></tr>
          </thead>
          <tbody>
            {channels.map((c) => (
              <tr key={c.id}>
                <td>{c.name}</td>
                <td><code>{c.type}</code></td>
                <td>{c.events && c.events.length ? c.events.join(", ") : <span className="muted">{t("all")}</span>}</td>
                <td>
                  {c.enabled ? t("enabled") : <span className="muted">{t("disabled")}</span>}
                  {status[c.id] && <div className="muted">{status[c.id]}</div>}
                </td>
                <td style={{ whiteSpace: "nowrap" }}>
                  <button onClick={() => test(c)}>{t("Test")}</button>{" "}
                  <button onClick={() => toggle(c)}>{c.enabled ? t("Disable") : t("Enable")}</button>{" "}
                  <button onClick={() => edit(c)}>{t("Edit")}</button>{" "}
                  <button className="danger" onClick={() => remove(c)}>{t("Delete")}</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <form onSubmit={submit} style={{ marginTop: 12 }}>
        <h3>{form.id === 0 ? t("New channel") : t('Edit "{name}"', { name: form.name })}</h3>
        <div className="grid2">
          <div>
            <label>{t("Name")}</label>
            <input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} required />
          </div>
          <div>
            <label>{t("Type")}</label>
            <select
              value={form.type}
              disabled={form.id !== 0}
              onChange={(e) => setForm({ ...form, type: e.target.value as ChannelType, config: {} })}
            >
              <option value="discord">Discord</option>
              <option value="webhook">Webhook</option>
              <option value="email">Email (SMTP)</option>
            </select>
          </div>
        </div>

        {FIELDS[form.type].map((fld) => {
          const configured = form.secrets[fld.key];
          if (fld.select) {
            return (
              <div key={fld.key}>
                <label>{t(fld.label)}</label>
                <select value={form.config[fld.key] ?? fld.select[0]} onChange={(e) => setConfig(fld.key, e.target.value)}>
                  {fld.select.map((o) => <option key={o} value={o}>{o}</option>)}
                </select>
              </div>
            );
          }
          return (
            <div key={fld.key}>
              <label>{t(fld.label)}{fld.secret && configured ? t(" (configured — leave blank to keep)") : ""}</label>
              <input
                type={fld.secret ? "password" : "text"}
                autoComplete={fld.secret ? "new-password" : "off"}
                placeholder={fld.placeholder}
                value={form.config[fld.key] ?? ""}
                onChange={(e) => setConfig(fld.key, e.target.value)}
              />
            </div>
          );
        })}

        <label style={{ marginTop: 8 }}>{t("Events (none selected = all)")}</label>
        <div className="row" style={{ flexWrap: "wrap", gap: 8 }}>
          {EVENT_TYPES.map((ev) => (
            <label key={ev} className="row" style={{ gap: 4 }}>
              <input
                type="checkbox"
                style={{ width: "auto" }}
                checked={form.events.includes(ev)}
                onChange={() => toggleEvent(ev)}
              />
              <code>{ev}</code>
            </label>
          ))}
        </div>

        <label className="row" style={{ marginTop: 8 }}>
          <input
            type="checkbox"
            style={{ width: "auto" }}
            checked={form.enabled}
            onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
          />
          &nbsp;{t("Enabled")}
        </label>

        {error && <div className="error">{error}</div>}
        <div className="row" style={{ marginTop: 12 }}>
          <button className="primary" disabled={busy || !form.name}>
            {busy ? t("Saving…") : form.id === 0 ? t("Add channel") : t("Save changes")}
          </button>
          {form.id !== 0 && <button type="button" onClick={reset}>{t("Cancel")}</button>}
        </div>
      </form>
    </div>
  );
}
