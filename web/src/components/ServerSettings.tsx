import { useId, FormEvent, useEffect, useState } from "react";
import { api, ApiError, FileEntry, Server, Template, TemplateVariable } from "../api";
import { useT } from "../i18n";
import { keepLines, keepPreview } from "../keep";
import { Combobox } from "./Combobox";
import { PortsEditor, PortRow, rowsToPorts } from "./PortsEditor";
import { RestartHint } from "./RestartHint";

// ServerSettings edits a running server's startup variables and resource limits.
// Both apply on the next reconcile, which restarts the server.
// canSwitchTemplate: changing what the server is -- another template -- is its
// owner's call or an administrator's; a subuser trusted with its settings may
// reinstall it as it is. canEditStartup: the startup command itself is the
// administrators' (servers permission), as the API has it.
export function ServerSettings({ server, onSaved, canSwitchTemplate, canEditStartup }: { server: Server; onSaved: (s: Server) => void; canSwitchTemplate: boolean; canEditStartup: boolean }) {
  const { t } = useT();
  const [loadedTemplate, setLoadedTemplate] = useState<Template | null>(null);
  const tmpl = loadedTemplate?.id === server.templateId ? loadedTemplate : null;

  useEffect(() => {
    let live = true;
    api.templates().then((ts) => {
      if (live) setLoadedTemplate(ts.find((t) => t.id === server.templateId) ?? null);
    }).catch(() => {});
    return () => { live = false; };
  }, [server.templateId]);

  const editable = (tmpl?.variables ?? []).filter((v) => v.editable);

  return (
    <>
    <RenameForm server={server} onSaved={onSaved} />
    <ReachesForm server={server} onSaved={onSaved} />
    <div className="card">
      <h2>{t("Startup & resources")}</h2>
      <p className="muted">{t("Edit this server's configuration. A ↻ marker appears on a pending change that will restart the server.")}</p>
      {editable.length > 0 && <Variables key={`${server.id}:${server.templateId}`} serverId={server.id} vars={editable} env={server.env ?? {}} onSaved={onSaved} />}
      {tmpl && <StartupForm server={server} template={tmpl} onSaved={onSaved} canEdit={canEditStartup} />}
      {tmpl && <ImageForm server={server} template={tmpl} onSaved={onSaved} />}
      <ResourcesForm server={server} onSaved={onSaved} />
      {tmpl && (tmpl.ports?.length ?? 0) === 0 && <ServerPorts server={server} onSaved={onSaved} />}
      {tmpl?.features?.includes("eula") && <EULAToggle server={server} onSaved={onSaved} />}
      {tmpl && <Reinstall server={server} current={tmpl} onSaved={onSaved} canSwitch={canSwitchTemplate} />}
    </div>
    </>
  );
}

// RenameForm changes the name shown for the server. The slug, and with it the
// server's Kubernetes objects and addresses, never changes.
function RenameForm({ server, onSaved }: { server: Server; onSaved: (s: Server) => void }) {
  const fieldId = useId();
  const { t } = useT();
  const [name, setName] = useState(server.displayName);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");
  useEffect(() => setName(server.displayName), [server.displayName]);
  const dirty = name.trim() !== server.displayName;

  async function submit(e: FormEvent) {
    e.preventDefault();
    setMsg("");
    setError("");
    setBusy(true);
    try {
      onSaved(await api.renameServer(server.id, name.trim()));
      setMsg(t("Name saved."));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form className="card" onSubmit={submit}>
      <h2>{t("Name")}</h2>
      <div className="row" style={{ gap: 8 }}>
        <input aria-describedby={`${fieldId}-rename-help`} value={name} maxLength={190} onChange={(e) => setName(e.target.value)} aria-label={t("Name")} style={{ flex: "1 1 200px", width: "auto" }} />
        <button className="primary" disabled={busy || !dirty || !name.trim()}>{busy ? t("Saving…") : t("Rename")}</button>
      </div>
      <p id={`${fieldId}-rename-help`} className="muted">{t("Only the displayed name changes: the server's address and ID stay the same.")}</p>
      {msg && <div className="notice">{msg}</div>}
      {error && <div className="error">{error}</div>}
    </form>
  );
}

// internalAddress is where another server of the cluster reaches this one.
function internalAddress(s: Server): string {
  const port = (s.ports ?? []).find((p) => p.primary) ?? (s.ports ?? [])[0];
  return port ? `server.${s.namespace}.svc.cluster.local:${port.port}` : "";
}

// ReachesForm chooses the servers this one may reach inside the cluster, which
// a game server otherwise cannot: the servers behind a Velocity or BungeeCord
// proxy, which then need not be on the internet at all.
function ReachesForm({ server, onSaved }: { server: Server; onSaved: (s: Server) => void }) {
  const { t } = useT();
  const [others, setOthers] = useState<Server[]>([]);
  const [chosen, setChosen] = useState<string[]>(server.reaches ?? []);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");
  useEffect(() => {
    api.servers()
      .then((ss) => setOthers(ss.filter((s) => s.id !== server.id && (s.clusterId ?? 0) === (server.clusterId ?? 0))))
      .catch(() => {});
  }, [server.id, server.clusterId]);
  // Polls decode a fresh array; only a change to the saved set resets the draft.
  const savedKey = JSON.stringify([...(server.reaches ?? [])].sort());
  useEffect(() => setChosen(JSON.parse(savedKey) as string[]), [server.id, savedKey]);

  const saved = server.reaches ?? [];
  const dirty = chosen.length !== saved.length || chosen.some((s) => !saved.includes(s));
  // A server reached before it was deleted is still listed, to be let go.
  const gone = saved.filter((slug) => !others.some((s) => s.slug === slug));

  function toggle(slug: string, on: boolean) {
    setChosen((c) => (on ? [...c, slug] : c.filter((s) => s !== slug)));
  }

  async function save() {
    setMsg("");
    setError("");
    setBusy(true);
    try {
      const saved = await api.setServerReaches(server.id, chosen);
      setChosen(saved.reaches ?? []);
      onSaved(saved);
      setMsg(t("Saved: it applies within a few seconds, without a restart."));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="card">
      <h2>{t("Reachable servers")}</h2>
      <p className="muted">
        {t("A game server cannot reach the others inside the cluster. Choose the ones this server may reach, such as the servers behind a Velocity or BungeeCord proxy: they then need not be exposed at all, and the proxy uses the address shown next to each.")}
      </p>
      {others.length === 0 && gone.length === 0 ? (
        <p className="muted">{t("No other server runs on this cluster.")}</p>
      ) : (
        <>
          {others.map((s) => (
            <label key={s.id} className="row" style={{ gap: 6, width: "auto" }}>
              <input
                type="checkbox"
                style={{ width: "auto" }}
                checked={chosen.includes(s.slug)}
                onChange={(e) => toggle(s.slug, e.target.checked)}
              />
              {s.displayName}
              {internalAddress(s) && <code className="muted">{internalAddress(s)}</code>}
            </label>
          ))}
          {gone.map((slug) => (
            <label key={slug} className="row" style={{ gap: 6, width: "auto" }}>
              <input type="checkbox" style={{ width: "auto" }} checked={chosen.includes(slug)} onChange={(e) => toggle(slug, e.target.checked)} />
              <span className="muted">{t("{slug} (deleted)", { slug })}</span>
            </label>
          ))}
          <button className="primary" style={{ marginTop: 8 }} onClick={save} disabled={busy || !dirty}>
            {busy ? t("Saving…") : t("Save")}
          </button>
        </>
      )}
      {msg && <div className="notice">{msg}</div>}
      {error && <div className="error">{error}</div>}
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

// What a reinstall does with the server's files: install over them, delete
// all but a list of paths first (a clean reinstall, how a modpack is
// updated), or delete them all.
type FilesMode = "keep" | "clean" | "wipe";

// Reinstall re-runs the install script, and can move the server to another
// template on the way -- another egg for the same game (Paper to Fabric, keeping
// the world) or another game -- and to another of its images. Only the owner or
// an administrator may switch (canSwitch), which is also who the API lets.
function Reinstall({ server, current, onSaved, canSwitch }: { server: Server; current: Template; onSaved: (s: Server) => void; canSwitch: boolean }) {
  const fieldId = useId();
  const { t } = useT();
  const [templates, setTemplates] = useState<Template[]>([]);
  const [slug, setSlug] = useState(current.slug);
  const [image, setImage] = useState(server.image);
  const [mode, setMode] = useState<FilesMode>("keep");
  // The paths a clean reinstall keeps: the server's last list, else what its
  // template offers. A template change offers the new template's.
  const offeredKeep = (x: Template) => (x.slug === current.slug && server.reinstallKeep?.length ? server.reinstallKeep : x.effectiveReinstallKeep ?? []);
  const [keepText, setKeepText] = useState(() => offeredKeep(current).join("\n"));
  // The top of the server's files, for the preview; null when it cannot be read.
  const [top, setTop] = useState<FileEntry[] | null>(null);
  const [values, setValues] = useState<Record<string, string>>({});
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api.templates().then(setTemplates).catch(() => {});
  }, []);
  // After a switch the page reloads the server with its new template.
  useEffect(() => {
    setSlug(current.slug);
    setImage(server.image);
  }, [current.slug, server.image]);
  // The preview needs the files: read them afresh each time a clean
  // reinstall is chosen. Without the files permission there is no preview.
  useEffect(() => {
    if (mode !== "clean") return;
    let live = true;
    api
      .listFiles(server.id, "")
      .then((f) => live && setTop(f))
      .catch(() => live && setTop(null));
    return () => {
      live = false;
    };
  }, [mode, server.id]);

  const target = templates.find((x) => x.slug === slug) ?? current;
  const switching = target.slug !== current.slug;
  const installs = !!target.install?.script;
  // The variables the switch cannot fill on its own: required by the new
  // template, without a default. The rest carry over or take their defaults,
  // and can be edited afterwards like any other.
  const needed = switching
    ? (target.variables ?? []).filter((v) => v.editable && v.required && !(v.default ?? "").trim())
    : [];

  function pick(next: string) {
    const x = templates.find((y) => y.slug === next);
    if (!x) return;
    setSlug(next);
    // Keep the current image when the new template offers it, as the API does.
    const images = x.images ?? [];
    const offered = images.some((i) => i.ref === server.image);
    setImage(offered ? server.image : (images.find((i) => i.default) ?? images[0])?.ref ?? "");
    const v: Record<string, string> = {};
    for (const nv of x.variables ?? []) {
      if (nv.editable && nv.required && !(nv.default ?? "").trim()) v[nv.envVariable] = nv.secret ? "" : server.env?.[nv.envVariable] ?? "";
    }
    setValues(v);
    if (!x.install?.script) setMode("keep");
    setKeepText(offeredKeep(x).join("\n"));
  }

  const keep = keepLines(keepText);
  const wipe = mode !== "keep";
  async function run() {
    const files =
      mode === "clean"
        ? t("Delete all of this server's files except the {n} paths kept, then re-run the install script? What is deleted cannot be recovered without a backup.", { n: keep.length })
        : mode === "wipe"
          ? t("Reinstall AND WIPE all data? This permanently deletes the server's files, then re-runs the install script.")
          : t("Reinstall this server? It re-runs the install script and restarts the server (data is kept).");
    const warning = switching
      ? t('Switch this server to "{name}"? Ports, resources and — unless wiped — its files are kept. Variables with the same name carry over; the others take the new template\'s defaults.', { name: target.name }) +
        (installs ? " " + t("The new template's install script runs on the next start.") : "") +
        (wipe ? " " + files : "")
      : files;
    if (!window.confirm(warning)) return;
    setBusy(true);
    setMsg("");
    setError("");
    try {
      const env: Record<string, string> = {};
      for (const [k, v] of Object.entries(values)) if (v.trim() !== "") env[k] = v;
      const res = await api.reinstallServer(server.id, {
        wipeData: wipe,
        ...(mode === "clean" ? { keep } : {}),
        ...(switching ? { template: target.slug } : {}),
        ...(image !== server.image ? { image } : {}),
        ...(switching && Object.keys(env).length ? { env } : {}),
      });
      let done = switching
        ? t('Switched to "{name}".', { name: target.name }) +
          " " +
          (res.status === "reinstalling" ? t("Its install runs on the next start.") : t("It has no install step: the image does the work at start."))
        : mode === "clean"
          ? t("Clean reinstall triggered — on the next start everything but the kept paths is deleted, then the install script runs.")
          : t("Reinstall triggered — the server will re-run its install script on the next start/reconcile.");
      if (res.reset.length) done += " " + t("Reset to the new template's defaults: {vars}.", { vars: res.reset.join(", ") });
      if (res.startupDropped) done += " " + t("Its own startup command was dropped: it runs the new template's.");
      setMsg(done);
      onSaved(await api.server(server.id));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  const blocked =
    (!switching && !installs) || (mode === "clean" && keep.length === 0) || needed.some((v) => !(values[v.envVariable] ?? "").trim() && !v.secret);
  const preview = mode === "clean" && top ? keepPreview(top, keep) : null;
  const names = (state: string) =>
    preview!.entries
      .filter((e) => e.state === state)
      .map((e) => e.name + (e.dir ? "/" : ""))
      .join(", ");
  return (
    <div style={{ marginTop: 12 }}>
      <h3>{canSwitch ? t("Reinstall or change template") : t("Reinstall")}</h3>
      <p className="muted">
        {t("Re-runs the template's install script, optionally on another template or image. Applied on the next reconcile, which restarts the server.")}
      </p>
      {canSwitch && (
        <>
          <label htmlFor={`${fieldId}-template`}>{t("Template")}</label>
          <Combobox id={`${fieldId}-template`}
            options={templates.map((x) => ({ value: x.slug, label: x.slug === current.slug ? `${x.name} ${t("(current)")}` : x.name }))}
            value={slug}
            placeholder={t("Search or select a template…")}
            emptyLabel={t("No templates match your search.")}
            onChange={pick}
          />
        </>
      )}
      {!switching && !installs && (
        <p className="muted">
          {canSwitch ? t("This template has no install step. Pick another template to switch to.") : t("This template has no install step.")}
        </p>
      )}
      <label htmlFor={`${fieldId}-image`}>{t("Image")}</label>
      <select id={`${fieldId}-image`} value={image} onChange={(e) => setImage(e.target.value)}>
        {(target.images ?? []).map((i) => (
          <option key={i.ref} value={i.ref}>
            {i.displayName} ({i.ref})
          </option>
        ))}
      </select>
      {needed.map((v) => (
        <div key={v.envVariable}>
          <label htmlFor={`${fieldId}-required-${v.envVariable}`}>
            {v.name} <code>{v.envVariable}</code> — {t("required by the new template")}
          </label>
          <input id={`${fieldId}-required-${v.envVariable}`}
            type={v.secret ? "password" : "text"}
            value={values[v.envVariable] ?? ""}
            placeholder={v.secret ? t("leave blank to keep the current value, if any") : ""}
            onChange={(e) => setValues({ ...values, [v.envVariable]: e.target.value })}
          />
        </div>
      ))}
      <div id={`${fieldId}-files-label`} style={{ display: "block", margin: "10px 0 4px", color: "var(--ink-muted)", fontSize: 13, marginTop: 8 }}>{t("The server's files")}</div>
      <div role="group" aria-labelledby={`${fieldId}-files-label`}>
      <label className="row" style={{ gap: 6 }}>
        <input type="radio" name={`${fieldId}-files-mode`} style={{ width: "auto" }} checked={mode === "keep"} onChange={() => setMode("keep")} />
        {t("Keep them all: the install runs over them")}
      </label>
      <label className="row" style={{ gap: 6 }}>
        <input type="radio" name={`${fieldId}-files-mode`} style={{ width: "auto" }} checked={mode === "clean"} disabled={!installs} onChange={() => setMode("clean")} />
        {t("Delete them all except the paths below — to update a modpack")}
      </label>
      {mode === "clean" && (
        <div style={{ marginLeft: 22 }}>
          <p id={`${fieldId}-keep-help`} className="muted">
            {t("The new version goes in clean: what the old one shipped and the new one does not — mods, configs, scripts — is gone, and the world stays. One path per line, from the server's files; * matches any name. The list is remembered for next time. A backup first is wise.")}
          </p>
          <textarea aria-label={t("Paths to keep")} aria-describedby={`${fieldId}-keep-help`}
            value={keepText}
            rows={Math.min(14, Math.max(4, keep.length + 1))}
            spellCheck={false}
            placeholder={"world*\nserver.properties"}
            onChange={(e) => setKeepText(e.target.value)}
            style={{ fontFamily: "monospace" }}
          />
          {keep.length === 0 && <div className="error">{t("List at least one path to keep, or delete all the files.")}</div>}
          {preview && (
            <div className="muted" style={{ marginTop: 6 }}>
              {names("kept") && (
                <div>
                  {t("Kept:")} <code>{names("kept")}</code>
                </div>
              )}
              {names("partly") && (
                <div>
                  {t("Kept in part, for the paths listed inside:")} <code>{names("partly")}</code>
                </div>
              )}
              {names("deleted") && (
                <div>
                  {t("Deleted:")} <code>{names("deleted")}</code>
                </div>
              )}
              {preview.unmatched.length > 0 && (
                <div className="error">
                  {t("Nothing matches, so nothing is kept for:")} <code>{preview.unmatched.join(", ")}</code>
                </div>
              )}
            </div>
          )}
        </div>
      )}
      <label className="row" style={{ gap: 6 }}>
        <input type="radio" name={`${fieldId}-files-mode`} style={{ width: "auto" }} checked={mode === "wipe"} disabled={!installs} onChange={() => setMode("wipe")} />
        {t("Delete them all")}
      </label>
      </div>
      {msg && <div className="notice">{msg}</div>}
      {error && <div className="error">{error}</div>}
      <button className={wipe ? "danger" : ""} style={{ marginTop: 8 }} onClick={run} disabled={busy || blocked}>
        {busy
          ? "…"
          : switching
            ? mode === "clean"
              ? t("Switch & clean reinstall")
              : wipe
                ? t("Switch & wipe")
                : t("Switch template")
            : mode === "clean"
              ? t("Clean reinstall")
              : wipe
                ? t("Reinstall & wipe")
                : t("Reinstall")}
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
  const fieldId = useId();
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
          <label htmlFor={`${fieldId}-variable-${v.envVariable}`}>{v.name || v.envVariable}{v.required ? " *" : ""}</label>
          {v.description && <p id={`${fieldId}-variable-help-${v.envVariable}`} className="muted" style={{ margin: "2px 0" }}>{v.description}</p>}
          {v.type === "enum" && v.options?.length ? (
            <select aria-describedby={!!v.description ? `${fieldId}-variable-help-${v.envVariable}` : undefined} id={`${fieldId}-variable-${v.envVariable}`} value={values[v.envVariable]} onChange={(e) => setValues({ ...values, [v.envVariable]: e.target.value })}>
              {v.options.map((o) => <option key={o} value={o}>{o}</option>)}
            </select>
          ) : (
            <input aria-describedby={!!v.description ? `${fieldId}-variable-help-${v.envVariable}` : undefined} id={`${fieldId}-variable-${v.envVariable}`}
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

// StartupForm shows the command the server's game starts with, and lets an
// administrator give the server one of its own, as Pterodactyl does: an
// argument its template has no variable for (TeamSpeak on MariaDB, a JVM flag
// for one server) took a copy of the template. Everyone else sees the command,
// and edits its variables above.
function StartupForm({ server, template, onSaved, canEdit }: { server: Server; template: Template; onSaved: (s: Server) => void; canEdit: boolean }) {
  const fieldId = useId();
  const { t } = useT();
  const own = server.startup ?? "";
  const effective = own || template.startup || "";
  const [value, setValue] = useState(effective);
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => setValue(effective), [effective]);
  const dirty = value.trim() !== effective.trim();
  if (!canEdit && !effective) return null;

  async function save(startup: string) {
    setMsg("");
    setError("");
    setBusy(true);
    try {
      const saved = await api.setServerStartup(server.id, startup);
      onSaved(saved);
      setMsg(saved.startup ? t("Startup command saved.") : t("The server runs its template's startup command again."));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        save(value);
      }}
      style={{ marginTop: 12 }}
    >
      <h3>
        {t("Startup command")} {own && <span className="badge warn">{t("custom")}</span>} {dirty && <RestartHint />}
      </h3>
      {canEdit ? (
        <textarea aria-describedby={`${fieldId}-startup-help`}
          value={value}
          rows={3}
          onChange={(e) => setValue(e.target.value)}
          aria-label={t("Startup command")}
          placeholder={t("Empty: the image's own entrypoint starts the server.")}
          style={{ fontFamily: "var(--font-mono)", fontSize: 12.5 }}
        />
      ) : (
        <pre className="log" style={{ maxHeight: 160 }}>{effective}</pre>
      )}
      <p id={`${fieldId}-startup-help`} className="muted">
        {own
          ? t("This server has a startup command of its own, set by an administrator: it no longer follows its template's.")
          : t("The template's startup command.")}{" "}
        {t("{{VARIABLE}} placeholders are filled in with the server's variables.")}
      </p>
      {own && template.startup && (
        <details>
          <summary className="muted">{t("The template's command")}</summary>
          <pre className="log" style={{ maxHeight: 160 }}>{template.startup}</pre>
        </details>
      )}
      {msg && <div className="notice">{msg}</div>}
      {error && <div className="error">{error}</div>}
      {canEdit && (
        <div className="row" style={{ marginTop: 8, gap: 8 }}>
          <button className="primary" disabled={busy || !dirty}>{busy ? t("Saving…") : t("Save startup command")}</button>
          {own && (
            <button type="button" disabled={busy} onClick={() => save("")}>
              {t("Use the template's")}
            </button>
          )}
        </div>
      )}
    </form>
  );
}

// ImageForm switches the image the server runs among its template's, as
// Pterodactyl's startup settings do: going from Java 21 to Java 25 took a
// reinstall, and the game was downloaded again. Shown when there is a choice.
function ImageForm({ server, template, onSaved }: { server: Server; template: Template; onSaved: (s: Server) => void }) {
  const { t } = useT();
  const [image, setImage] = useState(server.image);
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => setImage(server.image), [server.image]);
  // An image an administrator set off the list stays selectable as it is.
  const images = template.images ?? [];
  const options = images.some((i) => i.ref === server.image)
    ? images
    : [{ displayName: t("current"), ref: server.image }, ...images];
  if (options.length < 2) return null;
  const dirty = image !== server.image;

  async function submit(e: FormEvent) {
    e.preventDefault();
    setMsg("");
    setError("");
    setBusy(true);
    try {
      onSaved(await api.setServerImage(server.id, image));
      setMsg(t("Image saved."));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} style={{ marginTop: 12 }}>
      <h3>{t("Image")} {dirty && <RestartHint />}</h3>
      <select aria-label={t("Image")} value={image} onChange={(e) => setImage(e.target.value)}>
        {options.map((i) => (
          <option key={i.ref} value={i.ref}>
            {i.displayName} ({i.ref})
          </option>
        ))}
      </select>
      {msg && <div className="notice">{msg}</div>}
      {error && <div className="error">{error}</div>}
      <button className="primary" style={{ marginTop: 8 }} disabled={busy || !dirty}>{busy ? t("Saving…") : t("Save image")}</button>
    </form>
  );
}

function ResourcesForm({ server, onSaved }: { server: Server; onSaved: (s: Server) => void }) {
  const fieldId = useId();
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
        <div><label htmlFor={`${fieldId}-memory`}>{t("Memory (blank = unlimited)")}</label><input id={`${fieldId}-memory`} value={memory} onChange={(e) => setMemory(e.target.value)} placeholder="2Gi" /></div>
        <div><label htmlFor={`${fieldId}-cpu`}>{t("CPU (blank = unlimited)")}</label><input id={`${fieldId}-cpu`} value={cpu} onChange={(e) => setCpu(e.target.value)} placeholder="1000m" /></div>
      </div>
      {msg && <div className="notice">{msg}</div>}
      {error && <div className="error">{error}</div>}
      <button className="primary" style={{ marginTop: 8 }} disabled={busy}>{busy ? t("Saving…") : t("Save resources")}</button>
    </form>
  );
}
