import { FormEvent, useEffect, useState } from "react";
import { api, ApiError, Cluster, CreateServerRequest, ExposeType, Template, wakesOnMinecraftLogin } from "../api";
import { useT } from "../i18n";
import { Combobox } from "./Combobox";
import { PortRow, PortsEditor, portsToRows, PROTO_BOTH, rowsToPorts } from "./PortsEditor";

export function CreateServer({
  memoryRequired = false,
  canImportTemplates = false,
  onDone,
  onCancel,
}: {
  // The panel refuses a server without a memory limit from anyone but an
  // administrator: its pod could take all of its node's memory.
  memoryRequired?: boolean;
  // May import eggs: with no template yet, the form points there.
  canImportTemplates?: boolean;
  onDone: () => void;
  onCancel: () => void;
}) {
  const { t } = useT();
  const [templates, setTemplates] = useState<Template[]>([]);
  const [templatesLoaded, setTemplatesLoaded] = useState(false);
  const [tplSlug, setTplSlug] = useState("");
  const [name, setName] = useState("");
  const [image, setImage] = useState("");
  const [memory, setMemory] = useState("");
  const [cpu, setCpu] = useState("");
  const [size, setSize] = useState("10Gi");
  // Players reach a server on a node port unless told otherwise: ClusterIP,
  // the default until now, made a server nobody outside the cluster could join.
  const [expose, setExpose] = useState<ExposeType>("NodePort");
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
  // The primary row is the game's port and blank: the form asks for it.
  const [gamePortRequired, setGamePortRequired] = useState(false);
  const [start, setStart] = useState(true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  function selectTemplate(t: Template) {
    setTplSlug(t.slug);
    const images = t.images ?? [];
    const def = images.find((i) => i.default) || images[0];
    setImage(def ? def.ref : "");
    // Seed only editable variables: the form renders these, and they're what we
    // submit. Non-editable defaults (e.g. TYPE=PAPER) are applied server-side, so
    // sending them would trip the "variable is not editable" guard.
    const e: Record<string, string> = {};
    (t.variables ?? []).forEach((v) => {
      if (v.editable && v.default) e[v.envVariable] = v.default;
    });
    setEnv(e);
    // Pre-fill the ports editor: templates that declare no ports (imported eggs)
    // expose extra ports as variables (QUERY_PORT, RCON_PORT…). Seed those so the
    // user starts from the egg's ports instead of a blank row.
    let rows: PortRow[] = [];
    let primary = 0;
    let required = false;
    if (!t.ports?.length) {
      const sugg = portsToRows(t.suggestedPorts ?? []);
      const mc = wakesOnMinecraftLogin(t);
      if (sugg.primaryIdx >= 0) {
        // A variable holds the game's port.
        rows = sugg.rows;
        primary = sugg.primaryIdx;
      } else if (t.allocatedPort || mc) {
        // The game listens on the port it is given, which no variable holds:
        // 25565 for Minecraft Java; for another game the form asks, on TCP and
        // UDP as Wings exposes an allocation. Guessing it from a variable put
        // Counter-Strike 2 on its SourceTV port, on TCP only.
        rows = [mc ? { port: "25565", protocol: "TCP" } : { port: "", protocol: PROTO_BOTH }, ...sugg.rows];
        required = !mc;
      } else {
        // No port the egg knows of (a chat bot, say): a blank row, dropped
        // if it stays blank.
        rows = [{ port: "", protocol: PROTO_BOTH }];
      }
    }
    if (rows.length > 0) {
      setCustomPorts(rows);
      setPrimaryIdx(primary);
    }
    setGamePortRequired(required);
    // UDP servers can only auto-sleep via the transparent proxy, so default it on.
    setProxy(
      [...(t.ports ?? []).map((p) => p.protocol), ...rows.map((r) => r.protocol)].some((p) => p.toUpperCase() !== "TCP"),
    );
  }

  useEffect(() => {
    api
      .templates()
      .then((ts) => {
        setTemplates(ts);
        setTemplatesLoaded(true);
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
      };
      await api.createServer(body);
      onDone();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  const editable = (tpl?.variables ?? []).filter((v) => v.editable);
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

  // A new install has no template: servers are made from imported eggs, and
  // this form, empty, said nothing about it.
  if (templatesLoaded && templates.length === 0) {
    return (
      <div className="card">
        <div className="row">
          <h2>{t("New server")}</h2>
          <div className="spacer" />
          <button onClick={onCancel}>{t("Cancel")}</button>
        </div>
        {canImportTemplates ? (
          <>
            <p>
              {t("There is no template yet. A server is made from a template, and templates are the Pterodactyl or Pelican eggs you import: most games have one in Pelican's repositories.")}
            </p>
            <div className="row">
              <button type="button" className="primary" onClick={() => { window.location.hash = "#/admin/templates"; }}>
                {t("Import an egg")}
              </button>
              <a href="https://github.com/pelican-eggs" target="_blank" rel="noreferrer">{t("Pelican's egg repositories")}</a>
            </div>
          </>
        ) : (
          <p>{t("There is no template yet: an administrator has to import one, a Pterodactyl or Pelican egg, before a server can be created.")}</p>
        )}
      </div>
    );
  }

  return (
    <div className="card">
      <div className="row">
        <h2>{t("New server")}</h2>
        <div className="spacer" />
        <button onClick={onCancel}>{t("Cancel")}</button>
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
          {(tpl?.images ?? []).map((i) => (
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
              required={memoryRequired}
              placeholder={memoryRequired ? t("e.g. 4Gi") : t("e.g. 4Gi (optional)")}
              onChange={(e) => setMemory(e.target.value)}
            />
            {!memoryRequired && !memory.trim() && (
              <div className="muted" style={{ fontSize: 12 }}>
                {t("Without a limit the server may use all of its node's memory, and a Java server sizes itself from it.")}
              </div>
            )}
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
              {tpl?.allocatedPort
                ? t("The primary port is the one the game listens on: the egg hands it to the game as SERVER_PORT, so enter the game's usual port. The others come from the egg's variables.")
                : t("This template declares no ports; define them here and pick the primary (the port players connect to).")}
            </div>
            <PortsEditor
              ports={customPorts}
              primaryIdx={primaryIdx}
              requirePrimary={gamePortRequired}
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
              <option value="NodePort">{t("NodePort (node IP : allocated port)")}</option>
              <option value="LoadBalancer">{t("LoadBalancer (external IP)")}</option>
              <option value="ClusterIP">{t("ClusterIP (in-cluster only)")}</option>
            </select>
            <div className="muted" style={{ fontSize: 12 }}>
              {t("Ports: {ports}", { ports: effPorts.map((p) => `${p.port}/${p.protocol}`).join(", ") })}
            </div>
            <label className="row" style={{ marginTop: 8 }}>
              <input
                type="checkbox"
                style={{ width: "auto" }}
                checked={hibernate}
                onChange={(e) => setHibernate(e.target.checked)}
              />
              &nbsp;{t("Auto-sleep when idle (no players) after")}&nbsp;
              <input
                type="number"
                min={1}
                style={{ width: 70 }}
                value={idleMin}
                onChange={(e) => setIdleMin(Number(e.target.value))}
              />
              &nbsp;{t("min")}
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
                    &nbsp;{t("Wake when a player connects (TCP; first attempt reconnects)")}
                  </label>
                )}
                <label className="row" style={{ marginTop: 4 }}>
                  <input
                    type="checkbox"
                    style={{ width: "auto" }}
                    checked={proxy}
                    onChange={(e) => setProxy(e.target.checked)}
                  />
                  &nbsp;{t("Transparent proxy (TCP+UDP, no reconnect; required for UDP)")}
                </label>
                {!tcpOnly && !proxy && (
                  <div className="error" style={{ fontSize: 12 }}>
                    {t("UDP servers need the transparent proxy to auto-sleep.")}
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
                  <BoolSelect
                    value={env[v.envVariable]}
                    fallback={v.default}
                    onChange={(val) => setEnv({ ...env, [v.envVariable]: val })}
                  />
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
          &nbsp;{t("Start immediately")}
        </label>

        {error && <div className="error">{error}</div>}
        <button
          className="primary"
          style={{ marginTop: 16 }}
          disabled={busy || !name || !tplSlug}
        >
          {busy ? t("Creating…") : t("Create server")}
        </button>
      </form>
    </div>
  );
}

// BoolSelect is an egg's boolean variable. Eggs write one as 1/0 as often as
// true/false, and their scripts test for the form they use, so the choice keeps
// that form: a true/false choice showed "true" for a 0, and its "false" read as
// on to a script testing for "0" (Counter-Strike 2's RCON switch).
function BoolSelect({ value, fallback, onChange }: { value?: string; fallback?: string; onChange: (v: string) => void }) {
  const [on, off] = /^[01]$/.test(value || fallback || "") ? ["1", "0"] : ["true", "false"];
  return (
    <select value={value || off} onChange={(e) => onChange(e.target.value)}>
      <option value={on}>true</option>
      <option value={off}>false</option>
    </select>
  );
}
