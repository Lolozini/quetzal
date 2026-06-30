import { FormEvent, useEffect, useState } from "react";
import { api, ApiError, Cluster, CreateServerRequest, ExposeType, Template } from "../api";
import { useT } from "../i18n";

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

  function selectTemplate(t: Template) {
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
    setEnv(e);
    // Pre-fill the ports editor: templates that declare no ports (imported eggs)
    // expose extra ports as variables (QUERY_PORT, RCON_PORT…). Seed those so the
    // user starts from the egg's ports instead of a blank row.
    const sugg = t.ports?.length ? [] : t.suggestedPorts ?? [];
    if (sugg.length > 0) {
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

  const editable = tpl?.variables.filter((v) => v.editable) ?? [];
  // Templates with no ports (imported eggs) let the user define them per server,
  // matching Pterodactyl's per-server allocations.
  const tplPorts = tpl?.ports ?? [];
  const usingCustomPorts = tplPorts.length === 0;
  const effPorts = usingCustomPorts
    ? customPorts
        .map((p, i) => ({ port: Number(p.port), protocol: p.protocol, primary: i === primaryIdx, blank: p.port.trim() === "" }))
        .filter((p) => !p.blank)
        .map(({ blank, ...p }) => p)
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
      <form onSubmit={submit}>
        <label>{t("Template")}</label>
        <select
          value={tplSlug}
          onChange={(e) => {
            const t = templates.find((x) => x.slug === e.target.value);
            if (t) selectTemplate(t);
          }}
        >
          {templates.map((t) => (
            <option key={t.slug} value={t.slug}>
              {t.name}
            </option>
          ))}
        </select>
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
            {customPorts.map((p, i) => (
              <div className="row" key={i} style={{ gap: 6, marginTop: 4, alignItems: "center" }}>
                <input
                  type="number"
                  min={1}
                  max={65535}
                  style={{ width: 120 }}
                  value={p.port}
                  placeholder="25565"
                  onChange={(e) =>
                    setCustomPorts(customPorts.map((q, j) => (j === i ? { ...q, port: e.target.value } : q)))
                  }
                />
                <select
                  style={{ width: "auto" }}
                  value={p.protocol}
                  onChange={(e) =>
                    setCustomPorts(customPorts.map((q, j) => (j === i ? { ...q, protocol: e.target.value } : q)))
                  }
                >
                  <option value="TCP">TCP</option>
                  <option value="UDP">UDP</option>
                </select>
                <label className="muted" style={{ fontSize: 12, display: "flex", alignItems: "center", gap: 4 }}>
                  <input
                    type="radio"
                    name="primaryPort"
                    checked={i === primaryIdx}
                    onChange={() => setPrimaryIdx(i)}
                  />
                  {t("primary")}
                </label>
                {customPorts.length > 1 && (
                  <button
                    type="button"
                    onClick={() => {
                      setCustomPorts(customPorts.filter((_, j) => j !== i));
                      setPrimaryIdx((cur) => (i === cur ? 0 : cur > i ? cur - 1 : cur));
                    }}
                  >
                    {t("Remove")}
                  </button>
                )}
              </div>
            ))}
            <button
              type="button"
              style={{ marginTop: 4 }}
              onClick={() => setCustomPorts([...customPorts, { port: "", protocol: "TCP" }])}
            >
              {t("Add port")}
            </button>
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
