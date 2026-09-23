import { FormEvent, useEffect, useState } from "react";
import { api, ApiError, Cluster, CreateServerRequest, ExposeType, PterodactylInspect, Template } from "../api";
import { useT } from "../i18n";
import { Combobox } from "./Combobox";
import { PortsEditor, rowsToPorts } from "./PortsEditor";

export function CreateServer({
  onDone,
  onCancel,
}: {
  onDone: () => void;
  onCancel: () => void;
}) {
  const { t } = useT();
  const [templates, setTemplates] = useState<Template[]>([]);
  const [tplSlug, setTplSlug] = useState("");
  const [name, setName] = useState("");
  const [image, setImage] = useState("");
  const [memory, setMemory] = useState("");
  const [cpu, setCpu] = useState("");
  const [size, setSize] = useState("10Gi");
  const [expose, setExpose] = useState<ExposeType>("ClusterIP");
  const [clusters, setClusters] = useState<Cluster[]>([]);
  const [cluster, setCluster] = useState("");
  const [hibernate, setHibernate] = useState(false);
  const [idleMin, setIdleMin] = useState(15);
  const [wakeOnConnect, setWakeOnConnect] = useState(true);
  const [proxy, setProxy] = useState(false);
  const [env, setEnv] = useState<Record<string, string>>({});
  const [eulaAccepted, setEulaAccepted] = useState(false);
  // Per-server ports, used when the template declares none (imported eggs).
  const [customPorts, setCustomPorts] = useState<{ port: string; protocol: string }[]>([
    { port: "25565", protocol: "TCP" },
  ]);
  // Which custom-ports row is the primary (the port players connect to).
  const [primaryIdx, setPrimaryIdx] = useState(0);
  const [start, setStart] = useState(true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  // Import from Pterodactyl: the server's page address and a client API key.
  // Once inspected, the form is filled from the source and the create request
  // carries the source, so the new server receives its data.
  const [pteroOpen, setPteroOpen] = useState(false);
  const [pteroUrl, setPteroUrl] = useState("");
  const [pteroKey, setPteroKey] = useState("");
  const [pteroBusy, setPteroBusy] = useState(false);
  const [pteroError, setPteroError] = useState("");
  const [ptero, setPtero] = useState<PterodactylInspect | null>(null);

  function selectTemplate(t: Template, from: PterodactylInspect | null = ptero) {
    setTplSlug(t.slug);
    const def = t.images.find((i) => i.default) || t.images[0];
    setImage(def ? def.ref : "");
    // Seed only editable variables: the form renders these, and they're what we
    // submit. Non-editable defaults (e.g. TYPE=PAPER) are applied server-side, so
    // sending them would trip the "variable is not editable" guard.
    const e: Record<string, string> = {};
    t.variables.forEach((v) => {
      if (v.editable && v.default) e[v.envVariable] = v.default;
    });
    // Imported: the source's values win, for whichever template is picked (an
    // enum value the template does not offer keeps the default).
    if (from) {
      t.variables.forEach((v) => {
        const val = from.draft.variables[v.envVariable];
        if (!v.editable || val === undefined) return;
        if (v.type === "enum" && v.options && !v.options.includes(val)) return;
        e[v.envVariable] = val;
      });
      const src = from.source.dockerImage;
      if (src && t.images.some((i) => i.ref === src)) setImage(src);
    }
    setEnv(e);
    // Pre-fill the ports editor: templates that declare no ports (imported eggs)
    // expose extra ports as variables (QUERY_PORT, RCON_PORT…). Seed those so the
    // user starts from the egg's ports instead of a blank row.
    const sugg = t.ports?.length ? [] : t.suggestedPorts ?? [];
    if (!t.ports?.length && from?.draft.ports?.length) {
      // The source's allocations, default first.
      setCustomPorts(from.draft.ports.map((p) => ({ port: p.port, protocol: p.protocol })));
      setPrimaryIdx(0);
    } else if (sugg.length > 0) {
      setCustomPorts(sugg.map((p) => ({ port: String(p.port), protocol: (p.protocol || "TCP").toUpperCase() })));
      const pi = sugg.findIndex((p) => p.primary);
      setPrimaryIdx(pi >= 0 ? pi : 0);
    } else {
      setCustomPorts([{ port: "25565", protocol: "TCP" }]);
      setPrimaryIdx(0);
    }
    // UDP servers can only auto-sleep via the transparent proxy, so default it on.
    setProxy(
      [...(t.ports ?? []), ...sugg].some((p) => p.protocol.toUpperCase() === "UDP"),
    );
  }

  useEffect(() => {
    api
      .templates()
      .then((ts) => {
        setTemplates(ts);
        if (ts[0]) selectTemplate(ts[0]);
      })
      .catch((e) => setError(String(e)));
    api
      .clusters()
      .then((cs) => {
        setClusters(cs);
        const local = cs.find((c) => c.inCluster) || cs[0];
        if (local) setCluster(local.slug);
      })
      .catch(() => {});
  }, []);

  const tpl = templates.find((t) => t.slug === tplSlug);

  async function inspect() {
    setPteroBusy(true);
    setPteroError("");
    try {
      const res = await api.inspectPterodactyl({ url: pteroUrl.trim(), apiKey: pteroKey.trim() });
      setPtero(res);
      const d = res.draft;
      setName(d.name);
      setMemory(d.memory ?? "");
      setCpu(d.cpu ?? "");
      setSize(d.storage);
      // The egg's template when one matches; otherwise the one on screen, so
      // the values still land wherever the variable names agree.
      const target = templates.find((x) => x.slug === d.template) ?? templates.find((x) => x.slug === tplSlug);
      if (target) selectTemplate(target, res);
      // The data brings the server with it: installing on top would be wasted
      // at best. Start once the import is done.
      setStart(true);
    } catch (err) {
      setPtero(null);
      setPteroError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setPteroBusy(false);
    }
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const body: CreateServerRequest = {
        name,
        template: tplSlug,
        image: image || undefined,
        memory: memory || undefined,
        cpu: cpu || undefined,
        start,
        storage: {
          type: "pvc",
          size: size || undefined,
        },
        expose: { type: expose },
        hibernation: { enabled: hibernate, idleMinutes: idleMin, wakeOnConnect: wakeOnConnect && !proxy, proxy },
        cluster: cluster || undefined,
        env,
        eulaAccepted: tpl?.features?.includes("eula") ? eulaAccepted : undefined,
        ports:
          usingCustomPorts && effPorts.length > 0
            ? effPorts.map((p) => ({ port: p.port, protocol: p.protocol, primary: p.primary }))
            : undefined,
        pterodactyl: ptero ? { url: pteroUrl.trim(), apiKey: pteroKey.trim() } : undefined,
      };
      await api.createServer(body);
      onDone();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  const editable = tpl?.variables.filter((v) => v.editable) ?? [];
  // Templates with no ports (imported eggs) let the user define them per server,
  // matching Pterodactyl's per-server allocations.
  const tplPorts = tpl?.ports ?? [];
  const usingCustomPorts = tplPorts.length === 0;
  const effPorts = usingCustomPorts
    ? rowsToPorts(customPorts, primaryIdx)
    : tplPorts.map((p, i) => ({ port: p.port, protocol: p.protocol, primary: i === 0 }));
  const hasPorts = effPorts.length > 0;
  // Hibernation needs reliable idle detection, which today only works for TCP
  // (UDP players are invisible to the connection probe). Hide the toggle for any
  // server exposing a UDP port so it isn't enabled as a silent no-op.
  const tcpOnly = hasPorts && effPorts.every((p) => p.protocol.toUpperCase() !== "UDP");

  return (
    <div className="card">
      <div className="row">
        <h2>{t("New server")}</h2>
        <div className="spacer" />
        <button onClick={onCancel}>{t("Cancel")}</button>
      </div>
      <div className="notice" style={{ marginBottom: 12 }}>
        {!pteroOpen ? (
          <button type="button" onClick={() => setPteroOpen(true)}>
            {t("Import from Pterodactyl…")}
          </button>
        ) : (
          <>
            <strong>{t("Import from Pterodactyl")}</strong>
            <div className="muted" style={{ fontSize: 12 }}>
              {t("Paste the address of the server's page on the panel and a client API key (Account → API Credentials, ptlc_…). The form is filled from the server; on creation, the panel backs it up and its files are copied into the new server. The key is not stored.")}
            </div>
            <label>{t("Server page address")}</label>
            <input
              value={pteroUrl}
              placeholder="https://panel.example.com/server/1a2b3c4d"
              onChange={(e) => setPteroUrl(e.target.value)}
            />
            <label>{t("Client API key")}</label>
            <input
              type="password"
              autoComplete="off"
              value={pteroKey}
              placeholder="ptlc_…"
              onChange={(e) => setPteroKey(e.target.value)}
            />
            <div className="row" style={{ marginTop: 8 }}>
              <button type="button" disabled={pteroBusy || !pteroUrl || !pteroKey} onClick={inspect}>
                {pteroBusy ? t("Reading…") : t("Read the server")}
              </button>
              {ptero && (
                <button
                  type="button"
                  onClick={() => {
                    setPtero(null);
                    setPteroOpen(false);
                  }}
                >
                  {t("Do not import")}
                </button>
              )}
            </div>
            {pteroError && <div className="error">{pteroError}</div>}
            {ptero && (
              <div style={{ marginTop: 8, fontSize: 13 }}>
                {t("Importing {name} (egg {egg}). Players will need the new server's address.", {
                  name: ptero.source.name,
                  egg: ptero.source.egg || "?",
                })}
                {(ptero.warnings ?? []).length > 0 && (
                  <ul className="muted" style={{ margin: "4px 0 0 16px", padding: 0 }}>
                    {(ptero.warnings ?? []).map((w) => (
                      <li key={w}>{w}</li>
                    ))}
                  </ul>
                )}
              </div>
            )}
          </>
        )}
      </div>
      <form onSubmit={submit}>
        <label>{t("Template")}</label>
        <Combobox
          options={templates.map((x) => ({ value: x.slug, label: x.name }))}
          value={tplSlug}
          placeholder={t("Search or select a template…")}
          emptyLabel={t("No templates match your search.")}
          onChange={(slug) => {
            const x = templates.find((y) => y.slug === slug);
            if (x) selectTemplate(x);
          }}
        />
        {tpl?.description && <p className="muted">{tpl.description}</p>}

        {clusters.length > 1 && (
          <>
            <label>{t("Cluster")}</label>
            <select value={cluster} onChange={(e) => setCluster(e.target.value)}>
              {clusters.map((c) => (
                <option key={c.id} value={c.slug} disabled={!c.reachable}>
                  {c.name}
                  {c.inCluster ? " (local)" : ""}
                  {c.reachable ? "" : " — unreachable"}
                </option>
              ))}
            </select>
          </>
        )}

        <label>{t("Name")}</label>
        <input value={name} onChange={(e) => setName(e.target.value)} required autoFocus />

        <label>{t("Image")}</label>
        <select value={image} onChange={(e) => setImage(e.target.value)}>
          {tpl?.images.map((i) => (
            <option key={i.ref} value={i.ref}>
              {i.displayName} ({i.ref})
            </option>
          ))}
        </select>

        <div className="grid2">
          <div>
            <label>{t("Memory limit")}</label>
            <input
              value={memory}
              placeholder={t("e.g. 4Gi (optional)")}
              onChange={(e) => setMemory(e.target.value)}
            />
          </div>
          <div>
            <label>{t("CPU limit")}</label>
            <input value={cpu} placeholder={t("e.g. 2 or 1500m (optional)")} onChange={(e) => setCpu(e.target.value)} />
          </div>
          <div>
            <label>{t("Volume size")}</label>
            <input value={size} onChange={(e) => setSize(e.target.value)} placeholder="10Gi" />
          </div>
        </div>

        {usingCustomPorts && (
          <>
            <label>{t("Ports")}</label>
            <div className="muted" style={{ fontSize: 12 }}>
              {t("This template declares no ports; define them here and pick the primary (the port players connect to).")}
            </div>
            <PortsEditor
              ports={customPorts}
              primaryIdx={primaryIdx}
              onChange={(p, i) => {
                setCustomPorts(p);
                setPrimaryIdx(i);
              }}
            />
          </>
        )}

        {hasPorts && (
          <>
            <label>{t("Network exposure")}</label>
            <select value={expose} onChange={(e) => setExpose(e.target.value as ExposeType)}>
              <option value="ClusterIP">ClusterIP (in-cluster only)</option>
              <option value="NodePort">NodePort (node IP : allocated port)</option>
              <option value="LoadBalancer">LoadBalancer (external IP)</option>
            </select>
            <div className="muted" style={{ fontSize: 12 }}>
              Ports: {effPorts.map((p) => `${p.port}/${p.protocol}`).join(", ")}
            </div>
            <label className="row" style={{ marginTop: 8 }}>
              <input
                type="checkbox"
                style={{ width: "auto" }}
                checked={hibernate}
                onChange={(e) => setHibernate(e.target.checked)}
              />
              &nbsp;Auto-sleep when idle (no players) after&nbsp;
              <input
                type="number"
                min={1}
                style={{ width: 70 }}
                value={idleMin}
                onChange={(e) => setIdleMin(Number(e.target.value))}
              />
              &nbsp;min
            </label>
            {hibernate && (
              <>
                {tcpOnly && (
                  <label className="row" style={{ marginTop: 4 }}>
                    <input
                      type="checkbox"
                      style={{ width: "auto" }}
                      checked={wakeOnConnect && !proxy}
                      disabled={proxy}
                      onChange={(e) => setWakeOnConnect(e.target.checked)}
                    />
                    &nbsp;Wake when a player connects (TCP; first attempt reconnects)
                  </label>
                )}
                <label className="row" style={{ marginTop: 4 }}>
                  <input
                    type="checkbox"
                    style={{ width: "auto" }}
                    checked={proxy}
                    onChange={(e) => setProxy(e.target.checked)}
                  />
                  &nbsp;Transparent proxy (TCP+UDP, no reconnect; required for UDP)
                </label>
                {!tcpOnly && !proxy && (
                  <div className="error" style={{ fontSize: 12 }}>
                    UDP servers need the transparent proxy to auto-sleep.
                  </div>
                )}
              </>
            )}
          </>
        )}

        {editable.length > 0 && (
          <>
            <h3 style={{ marginTop: 16 }}>{t("Variables")}</h3>
            {editable.map((v) => (
              <div key={v.envVariable}>
                <label>
                  {v.name}
                  {v.required ? " *" : ""}
                </label>
                {v.type === "enum" && v.options ? (
                  <select
                    value={env[v.envVariable] ?? ""}
                    onChange={(e) => setEnv({ ...env, [v.envVariable]: e.target.value })}
                  >
                    {v.options.map((o) => (
                      <option key={o} value={o}>
                        {o}
                      </option>
                    ))}
                  </select>
                ) : v.type === "bool" ? (
                  <select
                    value={env[v.envVariable] ?? "false"}
                    onChange={(e) => setEnv({ ...env, [v.envVariable]: e.target.value })}
                  >
                    <option value="true">true</option>
                    <option value="false">false</option>
                  </select>
                ) : (
                  <input
                    type={v.secret ? "password" : "text"}
                    autoComplete={v.secret ? "new-password" : "off"}
                    value={env[v.envVariable] ?? ""}
                    onChange={(e) => setEnv({ ...env, [v.envVariable]: e.target.value })}
                  />
                )}
                {v.description && (
                  <div className="muted" style={{ fontSize: 12 }}>
                    {v.description}
                  </div>
                )}
              </div>
            ))}
          </>
        )}

        {tpl?.features?.includes("eula") && (
          <label className="row" style={{ marginTop: 12 }}>
            <input
              type="checkbox"
              style={{ width: "auto" }}
              checked={eulaAccepted}
              onChange={(e) => setEulaAccepted(e.target.checked)}
            />
            &nbsp;{t("I accept the")}&nbsp;
            <a href="https://aka.ms/MinecraftEULA" target="_blank" rel="noreferrer">
              {t("Minecraft EULA")}
            </a>
          </label>
        )}

        <label className="row" style={{ marginTop: 12 }}>
          <input
            type="checkbox"
            style={{ width: "auto" }}
            checked={start}
            onChange={(e) => setStart(e.target.checked)}
          />
          &nbsp;{ptero ? t("Start once the data is imported") : t("Start immediately")}
        </label>

        {error && <div className="error">{error}</div>}
        <button
          className="primary"
          style={{ marginTop: 16 }}
          disabled={busy || !name || !tplSlug}
        >
          {busy ? t("Creating…") : ptero ? t("Create and import") : t("Create server")}
        </button>
      </form>
    </div>
  );
}
