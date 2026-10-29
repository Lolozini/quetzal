import { useEffect, useId, useState } from "react";
import { api, errorMessage, Port, Server } from "../api";
import { useT } from "../i18n";

// ExposeAdmin edits what only an administrator may set on a server's exposure:
// the public address players are shown when a router or a DNS name stands
// between them and the cluster, and the annotations copied onto its Service
// (an external-dns name, a MetalLB pool, a cloud load balancer's options).
// Both were reachable through the API alone.

function portKey(p: Port): string {
  return `${p.port}/${(p.protocol || "").toUpperCase() === "UDP" ? "UDP" : "TCP"}`;
}

function annotationsText(a?: Record<string, string>): string {
  return Object.entries(a || {})
    .map(([k, v]) => `${k}=${v}`)
    .join("\n");
}

// parseAnnotations reads one key=value per line; it returns the line it could
// not read instead.
function parseAnnotations(text: string): { annotations: Record<string, string> } | { bad: string } {
  const annotations: Record<string, string> = {};
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line) continue;
    const i = line.indexOf("=");
    if (i <= 0) return { bad: line };
    annotations[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return { annotations };
}

export function ExposeAdmin({ server, onSaved }: { server: Server; onSaved: (s: Server) => void }) {
  const fieldId = useId();
  const { t } = useT();
  const ports = server.ports || [];
  const pub = server.expose?.public;
  const initialPorts = () => Object.fromEntries(ports.map((p) => [portKey(p), pub?.ports?.[portKey(p)] ? String(pub.ports[portKey(p)]) : ""]));
  const [host, setHost] = useState(pub?.host || "");
  const [publicPorts, setPublicPorts] = useState<Record<string, string>>(initialPorts);
  const [annotations, setAnnotations] = useState(annotationsText(server.expose?.annotations));
  const [msg, setMsg] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const saved = JSON.stringify([pub?.host || "", initialPorts(), annotationsText(server.expose?.annotations)]);
  useEffect(() => {
    setHost(pub?.host || "");
    setPublicPorts(initialPorts());
    setAnnotations(annotationsText(server.expose?.annotations));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [saved]);
  const dirty = JSON.stringify([host.trim(), publicPorts, annotations.trim()]) !== saved;

  async function save() {
    setMsg("");
    setError("");
    const parsed = parseAnnotations(annotations);
    if ("bad" in parsed) {
      setError(t("This line is not key=value: {line}", { line: parsed.bad }));
      return;
    }
    const wanted: Record<string, number> = {};
    for (const [k, v] of Object.entries(publicPorts)) {
      if (!v.trim()) continue;
      const n = Number(v);
      if (!Number.isInteger(n) || n < 1 || n > 65535) {
        setError(t("The public port for {port} must be between 1 and 65535.", { port: k }));
        return;
      }
      wanted[k] = n;
    }
    setBusy(true);
    try {
      const next = await api.setExpose(server.id, {
        annotations: parsed.annotations,
        public: { host: host.trim(), ports: wanted },
      });
      onSaved(next);
      setMsg(t("Saved. The address shown follows within a few seconds."));
    } catch (err) {
      setError(errorMessage(err, t));
    } finally {
      setBusy(false);
    }
  }

  return (
    <details style={{ marginTop: 8 }}>
      <summary className="muted">{t("Public address and Service annotations")}</summary>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
        style={{ marginTop: 8 }}
      >
        <label htmlFor={`${fieldId}-host`}>{t("Public host")}</label>
        <input id={`${fieldId}-host`} aria-describedby={`${fieldId}-host-help`}
          value={host}
          onChange={(e) => setHost(e.target.value)}
          placeholder="play.example.org"
          autoComplete="off"
          spellCheck={false}
        />
        <p id={`${fieldId}-host-help`} className="muted">
          {t("What players type when a router or a DNS name stands between them and the cluster. Shown in place of the address worked out; nothing in the cluster changes.")}
        </p>
        {ports.length > 0 && (
          <div className="kv" style={{ marginTop: 8 }}>
            {ports.map((p) => (
              <div key={portKey(p)} style={{ display: "contents" }}>
                <label className="k" htmlFor={`${fieldId}-port-${portKey(p)}`}>{t("Public port for {port}", { port: portKey(p) })}</label>
                <input id={`${fieldId}-port-${portKey(p)}`}
                  inputMode="numeric"
                  value={publicPorts[portKey(p)] || ""}
                  onChange={(e) => setPublicPorts({ ...publicPorts, [portKey(p)]: e.target.value })}
                  placeholder={p.nodePort ? String(p.nodePort) : String(p.port)}
                  style={{ maxWidth: 120 }}
                />
              </div>
            ))}
          </div>
        )}
        <label htmlFor={`${fieldId}-ann`} style={{ marginTop: 12, display: "block" }}>{t("Service annotations")}</label>
        <textarea id={`${fieldId}-ann`} aria-describedby={`${fieldId}-ann-help`}
          value={annotations}
          rows={3}
          onChange={(e) => setAnnotations(e.target.value)}
          placeholder="external-dns.alpha.kubernetes.io/hostname=play.example.org"
          spellCheck={false}
          style={{ fontFamily: "var(--font-mono)", fontSize: 12.5 }}
        />
        <p id={`${fieldId}-ann-help`} className="muted">
          {t("One key=value per line, copied onto the server's Service: an external-dns name, a MetalLB pool, a cloud load balancer's options. Leave empty to remove them.")}
        </p>
        {msg && <div className="notice">{msg}</div>}
        {error && <div className="error">{error}</div>}
        <div className="row" style={{ marginTop: 8, gap: 8 }}>
          <button className="primary" disabled={busy || !dirty}>{busy ? t("Saving…") : t("Save")}</button>
        </div>
      </form>
    </details>
  );
}
