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
    // UDP servers can only auto-sleep via the transparent proxy, so default it on.
    setProxy((t.ports ?? []).some((p) => p.protocol.toUpperCase() === "UDP"));
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
  // Hibernation needs reliable idle detection, which today only works for TCP
  // (UDP players are invisible to the connection probe). Hide the toggle for any
  // server exposing a UDP port so it isn't enabled as a silent no-op.
  const tcpOnly =
    !!tpl?.ports && tpl.ports.length > 0 && tpl.ports.every((p) => p.protocol.toUpperCase() !== "UDP");

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

        {tpl?.ports && tpl.ports.length > 0 && (
          <>
            <label>{t("Network exposure")}</label>
            <select value={expose} onChange={(e) => setExpose(e.target.value as ExposeType)}>
              <option value="ClusterIP">ClusterIP (in-cluster only)</option>
              <option value="NodePort">NodePort (node IP : allocated port)</option>
              <option value="LoadBalancer">LoadBalancer (external IP)</option>
            </select>
            <div className="muted" style={{ fontSize: 12 }}>
              Ports:{" "}
              {tpl.ports.map((p) => `${p.port}/${p.protocol}`).join(", ")}
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
