import { FormEvent, useEffect, useState } from "react";
import { api, ApiError, Server, Template, TemplateVariable } from "../api";

// ServerSettings edits a running server's startup variables and resource limits.
// Both apply on the next reconcile, which restarts the server.
export function ServerSettings({ server, onSaved }: { server: Server; onSaved: (s: Server) => void }) {
  const [tmpl, setTmpl] = useState<Template | null>(null);

  useEffect(() => {
    api.templates().then((ts) => setTmpl(ts.find((t) => t.id === server.templateId) ?? null)).catch(() => {});
  }, [server.templateId]);

  const editable = (tmpl?.variables ?? []).filter((v) => v.editable);

  return (
    <div className="card">
      <h2>Startup &amp; resources</h2>
      <p className="muted">Changes apply on the next reconcile, which restarts the server.</p>
      {editable.length > 0 && <Variables serverId={server.id} vars={editable} env={server.env ?? {}} onSaved={onSaved} />}
      <ResourcesForm server={server} onSaved={onSaved} />
      {tmpl?.install?.script && <Reinstall serverId={server.id} />}
    </div>
  );
}

function Reinstall({ serverId }: { serverId: number }) {
  const [wipe, setWipe] = useState(false);
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function run() {
    const warning = wipe
      ? "Reinstall AND WIPE all data? This permanently deletes the server's files, then re-runs the install script."
      : "Reinstall this server? It re-runs the install script and restarts the server (data is kept).";
    if (!window.confirm(warning)) return;
    setBusy(true);
    setMsg("");
    setError("");
    try {
      await api.reinstallServer(serverId, wipe);
      setMsg("Reinstall triggered — the server will re-run its install script on the next start/reconcile.");
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div style={{ marginTop: 12 }}>
      <h3>Reinstall</h3>
      <p className="muted">Re-runs the template's install script. Applied on the next reconcile, which restarts the server.</p>
      <label className="row" style={{ gap: 6 }}>
        <input type="checkbox" style={{ width: "auto" }} checked={wipe} onChange={(e) => setWipe(e.target.checked)} />
        Also wipe the data volume (delete all files first)
      </label>
      {msg && <div className="notice">{msg}</div>}
      {error && <div className="error">{error}</div>}
      <button className={wipe ? "danger" : ""} style={{ marginTop: 8 }} onClick={run} disabled={busy}>
        {busy ? "…" : wipe ? "Reinstall & wipe" : "Reinstall"}
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
  // Seed each field: current value, else the variable default. Secrets start
  // blank (their value isn't returned); blank means "keep the stored secret".
  const [values, setValues] = useState<Record<string, string>>(() => {
    const v: Record<string, string> = {};
    for (const x of vars) v[x.envVariable] = x.secret ? "" : (env[x.envVariable] ?? x.default ?? "");
    return v;
  });
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
      setMsg("Variables saved.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} style={{ marginTop: 12 }}>
      <h3>Variables</h3>
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
              placeholder={v.secret ? "•••••• (leave blank to keep)" : v.default}
              autoComplete={v.secret ? "new-password" : "off"}
              onChange={(e) => setValues({ ...values, [v.envVariable]: e.target.value })}
            />
          )}
        </div>
      ))}
      {msg && <div className="notice">{msg}</div>}
      {error && <div className="error">{error}</div>}
      <button className="primary" style={{ marginTop: 8 }} disabled={busy}>{busy ? "Saving…" : "Save variables"}</button>
    </form>
  );
}

function ResourcesForm({ server, onSaved }: { server: Server; onSaved: (s: Server) => void }) {
  const [memory, setMemory] = useState(server.resources.memory ?? "");
  const [cpu, setCpu] = useState(server.resources.cpu ?? "");
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
      setMsg("Resources saved.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} style={{ marginTop: 12 }}>
      <h3>Resource limits</h3>
      <div className="grid2">
        <div><label>Memory (blank = unlimited)</label><input value={memory} onChange={(e) => setMemory(e.target.value)} placeholder="2Gi" /></div>
        <div><label>CPU (blank = unlimited)</label><input value={cpu} onChange={(e) => setCpu(e.target.value)} placeholder="1000m" /></div>
      </div>
      {msg && <div className="notice">{msg}</div>}
      {error && <div className="error">{error}</div>}
      <button className="primary" style={{ marginTop: 8 }} disabled={busy}>{busy ? "Saving…" : "Save resources"}</button>
    </form>
  );
}
