import { useId } from "react";
import { useT } from "../i18n";

export interface PortRow {
  port: string;
  protocol: string;
}

// PROTO_BOTH is the editor-only "same port on TCP and UDP" choice (e.g. a
// Minecraft game port that also serves the UDP query). It isn't a real protocol:
// rowsToPorts expands it into a TCP and a UDP port on the same number.
export const PROTO_BOTH = "TCP/UDP";

export interface PortIO {
  port: number;
  protocol: string;
  primary: boolean;
}

// rowsToPorts turns editor rows (+ the primary row index) into API ports: blank
// rows are dropped, and a "TCP/UDP" row becomes a TCP and a UDP port on the same
// number (the TCP side carries the primary flag, since the game handshake is TCP).
export function rowsToPorts(rows: PortRow[], primaryIdx: number): PortIO[] {
  const out: PortIO[] = [];
  rows.forEach((r, i) => {
    if (r.port.trim() === "") return;
    const port = Number(r.port);
    const primary = i === primaryIdx;
    if (r.protocol === PROTO_BOTH) {
      out.push({ port, protocol: "TCP", primary });
      out.push({ port, protocol: "UDP", primary: false });
    } else {
      out.push({ port, protocol: r.protocol.toUpperCase(), primary });
    }
  });
  return out;
}

/**
 * PortsEditor is the per-server ports editor (number + TCP/UDP + a "primary"
 * radio, add/remove rows). Controlled: the parent owns the rows and the primary
 * index. Shared by the create form and the server settings so both behave the
 * same.
 */
export function PortsEditor({
  ports,
  primaryIdx,
  onChange,
}: {
  ports: PortRow[];
  primaryIdx: number;
  onChange: (ports: PortRow[], primaryIdx: number) => void;
}) {
  const { t } = useT();
  const groupName = useId(); // unique radio group per editor instance

  const setRow = (i: number, patch: Partial<PortRow>) =>
    onChange(
      ports.map((q, j) => (j === i ? { ...q, ...patch } : q)),
      primaryIdx,
    );

  function removeRow(i: number) {
    const next = ports.filter((_, j) => j !== i);
    const nextPrimary = i === primaryIdx ? 0 : primaryIdx > i ? primaryIdx - 1 : primaryIdx;
    onChange(next, nextPrimary);
  }

  return (
    <>
      {ports.map((p, i) => (
        <div className="row" key={i} style={{ gap: 6, marginTop: 4, alignItems: "center" }}>
          <input
            type="number"
            min={1}
            max={65535}
            style={{ width: 120 }}
            value={p.port}
            placeholder="25565"
            onChange={(e) => setRow(i, { port: e.target.value })}
          />
          <select
            style={{ width: "auto" }}
            value={p.protocol}
            onChange={(e) => setRow(i, { protocol: e.target.value })}
          >
            <option value="TCP">TCP</option>
            <option value="UDP">UDP</option>
            <option value={PROTO_BOTH}>TCP / UDP</option>
          </select>
          <label className="muted" style={{ fontSize: 12, display: "flex", alignItems: "center", gap: 4 }}>
            <input
              type="radio"
              name={groupName}
              checked={i === primaryIdx}
              onChange={() => onChange(ports, i)}
            />
            {t("primary")}
          </label>
          {ports.length > 1 && (
            <button type="button" onClick={() => removeRow(i)}>
              {t("Remove")}
            </button>
          )}
        </div>
      ))}
      <button
        type="button"
        style={{ marginTop: 4 }}
        onClick={() => onChange([...ports, { port: "", protocol: "TCP" }], primaryIdx)}
      >
        {t("Add port")}
      </button>
      <p className="muted" style={{ fontSize: 12, marginTop: 4 }}>
        {t("Pick TCP / UDP for a port that needs both (e.g. a game port that also serves a UDP query).")}
      </p>
    </>
  );
}
