import { FormEvent, useEffect, useState } from "react";
import { api, ApiError, Server, Template, TemplateVariable } from "../api";
import { useT } from "../i18n";
import { PortsEditor, PortRow, rowsToPorts } from "./PortsEditor";
import { RestartHint } from "./RestartHint";

// ServerSettings edits a running server's startup variables and resource limits.
// Both apply on the next reconcile, which restarts the server.
export function ServerSettings({ server, onSaved }: { server: Server; onSaved: (s: Server) => void }) {
  const { t } = useT();
  const [tmpl, setTmpl] = useState<Template | null>(null);

  useEffect(() => {
    api.templates().then((ts) => setTmpl(ts.find((t) => t.id === server.templateId) ?? null)).catch(() => {});
  }, [server.templateId]);

  const editable = (tmpl?.variables ?? []).filter((v) => v.editable);

  return (
    <div className="card">
      <h2>{t("Startup & resources")}</h2>
      <p className="muted">{t("Edit this server's configuration. A ↻ marker appears on a pending change that will restart the server.")}</p>
      {editable.length > 0 && <Variables serverId={server.id} vars={editable} env={server.env ?? {}} onSaved={onSaved} />}
      <ResourcesForm server={server} onSaved={onSaved} />
      {tmpl && (tmpl.ports?.length ?? 0) === 0 && <ServerPorts server={server} onSaved={onSaved} />}
      {tmpl?.features?.includes("eula") && <EULAToggle server={server} onSaved={onSaved} />}
      {tmpl?.install?.script && <Reinstall serverId={server.id} />}
    </div>
  );
}

// EULAToggle accepts/revokes the Minecraft EULA for templates with the "eula"
// egg feature; on accept the controller writes eula.txt=true at next start.
function EULAToggle({ server, onSaved }: { server: Server; onSaved: (s: Server) => void }) {
  const { t } = useT();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function toggle(accepted: boolean) {
    setBusy(true);
    setError("");
    try {
      onSaved(await api.setEULA(server.id, accepted));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <div style={{ marginTop: 12 }}>
      <h3>{t("Minecraft EULA")}</h3>
      <label className="row" style={{ gap: 6 }}>
        <input
          type="checkbox"
          style={{ width: "auto" }}
          checked={!!server.eulaAccepted}
          disabled={busy}
          onChange={(e) => toggle(e.target.checked)}
        />
        {t("I accept the")}&nbsp;
        <a href="https://aka.ms/MinecraftEULA" target="_blank" rel="noreferrer">{t("Minecraft EULA")}</a>
      </label>
      <p className="muted">{t("Required for the server to start; applied on the next reconcile.")}</p>
      {error && <div className="error">{error}</div>}
    </div>
  );
}

// ServerPorts edits a server's per-server ports (imported eggs allocate ports
// per server, not in the egg). Saving reallocates node ports if needed and
// restarts the server on the next reconcile.
function ServerPorts({ server, onSaved }: { server: Server; onSaved: (s: Server) => void }) {
  const { t } = useT();
  const init = (server.ports ?? []).map((p) => ({ port: String(p.port), protocol: (p.protocol || "TCP").toUpperCase() }));
  const [rows, setRows] = useState<PortRow[]>(init.length ? init : [{ port: "", protocol: "TCP" }]);
  const initPrimary = (server.ports ?? []).findIndex((p) => p.primary);
  const [primaryIdx, setPrimaryIdx] = useState(initPrimary >= 0 ? initPrimary : 0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [msg, setMsg] = useState("");

  // Dirty when the current ports differ from what's saved on the server, so the
  // restart hint only shows once there's a pending change. Compare on the
  // expanded, order-independent form so a "TCP/UDP" row equals its saved TCP+UDP
  // pair.
  const sig = (ps: { port: number | string; protocol: string; primary?: boolean }[]) =>
    JSON.stringify(
      ps
        .map((p) => ({ port: Number(p.port), protocol: p.protocol.toUpperCase(), primary: !!p.primary }))
        .sort((a, b) => a.port - b.port || a.protocol.localeCompare(b.protocol)),
    );
  const dirty = sig(rowsToPorts(rows, primaryIdx)) !== sig(server.ports ?? []);

  async function save() {
    setBusy(true);
    setError("");
    setMsg("");
    try {
      const ports = rowsToPorts(rows, primaryIdx);
      onSaved(await api.setServerPorts(server.id, ports));
      setMsg(t("Ports saved; the server restarts to apply."));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div style={{ marginTop: 12 }}>
      <h3>{t("Ports")} {dirty && <RestartHint />}</h3>
      <p className="muted" style={{ fontSize: 12 }}>
        {t("The ports this server exposes; pick the primary (the port players connect to).")}
      </p>
      <PortsEditor ports={rows} primaryIdx={primaryIdx} onChange={(p, i) => { setRows(p); setPrimaryIdx(i); }} />
      {error && <div className="error">{error}</div>}
      {msg && <div className="notice">{msg}</div>}
      <button className="primary" style={{ marginTop: 8 }} onClick={save} disabled={busy}>
        {busy ? t("Saving…") : t("Save ports")}
      </button>
    </div>
  );
}

function Reinstall({ serverId }: { serverId: number }) {
  const { t } = useT();
  const [wipe, setWipe] = useState(false);
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function run() {
    const warning = wipe
      ? t("Reinstall AND WIPE all data? This permanently deletes the server's files, then re-runs the install script.")
      : t("Reinstall this server? It re-runs the install script and restarts the server (data is kept).");
    if (!window.confirm(warning)) return;
    setBusy(true);
    setMsg("");
    setError("");
    try {
      await api.reinstallServer(serverId, wipe);
      setMsg(t("Reinstall triggered — the server will re-run its install script on the next start/reconcile."));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div style={{ marginTop: 12 }}>
      <h3>{t("Reinstall")}</h3>
      <p className="muted">{t("Re-runs the template's install script. Applied on the next reconcile, which restarts the server.")}</p>
      <label className="row" style={{ gap: 6 }}>
        <input type="checkbox" style={{ width: "auto" }} checked={wipe} onChange={(e) => setWipe(e.target.checked)} />
        {t("Also wipe the data volume (delete all files first)")}
      </label>
      {msg && <div className="notice">{msg}</div>}
      {error && <div className="error">{error}</div>}
      <button className={wipe ? "danger" : ""} style={{ marginTop: 8 }} onClick={run} disabled={busy}>
        {busy ? "…" : wipe ? t("Reinstall & wipe") : t("Reinstall")}
      </button>
    </div>
  );
}

function Variables({
  serverId,
  vars,
  env,
  onSaved,
}: {
  serverId: number;
  vars: TemplateVariable[];
  env: Record<string, string>;
  onSaved: (s: Server) => void;
}) {
  const { t } = useT();
  // Seed each field: current value, else the variable default. Secrets start
  // blank (their value isn't returned); blank means "keep the stored secret".
  const [values, setValues] = useState<Record<string, string>>(() => {
    const v: Record<string, string> = {};
    for (const x of vars) v[x.envVariable] = x.secret ? "" : (env[x.envVariable] ?? x.default ?? "");
    return v;
  });
  // Baseline for the dirty check: the values as last seeded/saved. The restart
  // hint shows only while there are unsaved edits.
  const [saved, setSaved] = useState(values);
  const dirty = Object.keys(values).some((k) => values[k] !== saved[k]);
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setMsg("");
    setError("");
    setBusy(true);
    try {
      const s = await api.setServerEnv(serverId, values);
      onSaved(s);
      setSaved(values);
      setMsg(t("Variables saved."));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} style={{ marginTop: 12 }}>
      <h3>{t("Variables")} {dirty && <RestartHint />}</h3>
      {vars.map((v) => (
        <div key={v.envVariable} style={{ marginBottom: 8 }}>
          <label>{v.name || v.envVariable}{v.required ? " *" : ""}</label>
          {v.description && <p className="muted" style={{ margin: "2px 0" }}>{v.description}</p>}
          {v.type === "enum" && v.options?.length ? (
            <select value={values[v.envVariable]} onChange={(e) => setValues({ ...values, [v.envVariable]: e.target.value })}>
              {v.options.map((o) => <option key={o} value={o}>{o}</option>)}
            </select>
          ) : (
            <input
              type={v.secret ? "password" : "text"}
              value={values[v.envVariable]}
              placeholder={v.secret ? t("•••••• (leave blank to keep)") : v.default}
              autoComplete={v.secret ? "new-password" : "off"}
              onChange={(e) => setValues({ ...values, [v.envVariable]: e.target.value })}
            />
          )}
        </div>
      ))}
      {msg && <div className="notice">{msg}</div>}
      {error && <div className="error">{error}</div>}
      <button className="primary" style={{ marginTop: 8 }} disabled={busy}>{busy ? t("Saving…") : t("Save variables")}</button>
    </form>
  );
}

function ResourcesForm({ server, onSaved }: { server: Server; onSaved: (s: Server) => void }) {
  const { t } = useT();
  const [memory, setMemory] = useState(server.resources.memory ?? "");
  const [cpu, setCpu] = useState(server.resources.cpu ?? "");
  const dirty = memory.trim() !== (server.resources.memory ?? "") || cpu.trim() !== (server.resources.cpu ?? "");
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setMsg("");
    setError("");
    setBusy(true);
    try {
      const s = await api.setServerResources(server.id, { memory: memory.trim(), cpu: cpu.trim() });
      onSaved(s);
      setMsg(t("Resources saved."));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} style={{ marginTop: 12 }}>
      <h3>{t("Resource limits")} {dirty && <RestartHint />}</h3>
      <div className="grid2">
        <div><label>{t("Memory (blank = unlimited)")}</label><input value={memory} onChange={(e) => setMemory(e.target.value)} placeholder="2Gi" /></div>
        <div><label>{t("CPU (blank = unlimited)")}</label><input value={cpu} onChange={(e) => setCpu(e.target.value)} placeholder="1000m" /></div>
      </div>
      {msg && <div className="notice">{msg}</div>}
      {error && <div className="error">{error}</div>}
      <button className="primary" style={{ marginTop: 8 }} disabled={busy}>{busy ? t("Saving…") : t("Save resources")}</button>
    </form>
  );
}
