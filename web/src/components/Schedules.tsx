import { FormEvent, useEffect, useState } from "react";
import { api, ApiError, Schedule, ScheduleAction, ScheduleInput } from "../api";

const ACTIONS: ScheduleAction[] = ["start", "stop", "restart", "command", "backup"];

export function Schedules({ id }: { id: number }) {
  const [list, setList] = useState<Schedule[]>([]);
  const [error, setError] = useState("");
  const [name, setName] = useState("");
  const [cron, setCron] = useState("0 5 * * *");
  const [action, setAction] = useState<ScheduleAction>("restart");
  const [payload, setPayload] = useState("");
  const [busy, setBusy] = useState(false);

  async function load() {
    try {
      setList(await api.schedules(id));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
    const t = setInterval(load, 5000);
    return () => clearInterval(t);
  }, [id]);

  async function add(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const body: ScheduleInput = { name, cron, action, payload: action === "command" ? payload : undefined, enabled: true };
      await api.createSchedule(id, body);
      setName("");
      setPayload("");
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function toggle(s: Schedule) {
    setError("");
    try {
      await api.updateSchedule(id, s.id, {
        name: s.name, cron: s.cron, action: s.action, payload: s.payload, enabled: !s.enabled,
      });
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function remove(s: Schedule) {
    if (!window.confirm(`Delete schedule "${s.name}"?`)) return;
    try {
      await api.deleteSchedule(id, s.id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <div className="card">
      <h3>Scheduled tasks</h3>
      {list.length === 0 ? (
        <p className="muted">No schedules yet.</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Cron</th>
              <th>Action</th>
              <th>Next run</th>
              <th>Last</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {list.map((s) => (
              <tr key={s.id}>
                <td>{s.name}</td>
                <td><code>{s.cron}</code></td>
                <td>{s.action}{s.action === "command" && s.payload ? `: ${s.payload}` : ""}</td>
                <td>{s.enabled ? fmt(s.nextRun) : "—"}</td>
                <td title={s.lastStatus}>{s.lastRun ? fmt(s.lastRun) : "never"}</td>
                <td style={{ whiteSpace: "nowrap" }}>
                  <button onClick={() => toggle(s)}>{s.enabled ? "Disable" : "Enable"}</button>{" "}
                  <button className="danger" onClick={() => remove(s)}>Delete</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <form onSubmit={add} style={{ marginTop: 12 }}>
        <div className="grid2">
          <div>
            <label>Name</label>
            <input value={name} onChange={(e) => setName(e.target.value)} required placeholder="nightly restart" />
          </div>
          <div>
            <label>Cron (5 fields)</label>
            <input value={cron} onChange={(e) => setCron(e.target.value)} required placeholder="0 5 * * *" />
          </div>
        </div>
        <div className="grid2">
          <div>
            <label>Action</label>
            <select value={action} onChange={(e) => setAction(e.target.value as ScheduleAction)}>
              {ACTIONS.map((a) => <option key={a} value={a}>{a}</option>)}
            </select>
          </div>
          {action === "command" && (
            <div>
              <label>Command (sent to console)</label>
              <input value={payload} onChange={(e) => setPayload(e.target.value)} placeholder="say restarting soon" />
            </div>
          )}
        </div>
        {error && <div className="error">{error}</div>}
        <button className="primary" style={{ marginTop: 12 }} disabled={busy || !name || !cron}>
          {busy ? "Adding…" : "Add schedule"}
        </button>
      </form>
    </div>
  );
}

function fmt(iso?: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  return isNaN(d.getTime()) ? "—" : d.toLocaleString();
}
