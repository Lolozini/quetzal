import { FormEvent, useEffect, useState } from "react";
import { api, ApiError, Schedule, ScheduleAction, ScheduleInput, ScheduleTask } from "../api";
import { useT } from "../i18n";

const ACTIONS: ScheduleAction[] = ["start", "stop", "restart", "command", "backup"];

// chainOf normalizes a schedule into its task list (legacy single-action
// schedules carry action/payload instead of tasks).
function chainOf(s: Schedule): ScheduleTask[] {
  if (s.tasks && s.tasks.length) return s.tasks;
  if (s.action) return [{ action: s.action, payload: s.payload, timeOffset: 0 }];
  return [];
}

function newTask(): ScheduleTask {
  return { action: "restart", timeOffset: 0 };
}

export function Schedules({ id }: { id: number }) {
  const { t } = useT();
  const [list, setList] = useState<Schedule[]>([]);
  const [error, setError] = useState("");
  const [name, setName] = useState("");
  const [cron, setCron] = useState("0 5 * * *");
  const [tasks, setTasks] = useState<ScheduleTask[]>([newTask()]);
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

  function patchTask(i: number, patch: Partial<ScheduleTask>) {
    setTasks((ts) => ts.map((t, j) => (j === i ? { ...t, ...patch } : t)));
  }
  function addTask() {
    setTasks((ts) => [...ts, newTask()]);
  }
  function removeTask(i: number) {
    setTasks((ts) => (ts.length > 1 ? ts.filter((_, j) => j !== i) : ts));
  }

  async function add(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const clean = tasks.map((t) => ({
        action: t.action,
        payload: t.action === "command" ? t.payload : undefined,
        timeOffset: Number(t.timeOffset) || 0,
        continueOnFailure: t.continueOnFailure || undefined,
      }));
      const body: ScheduleInput = { name, cron, tasks: clean, enabled: true };
      await api.createSchedule(id, body);
      setName("");
      setTasks([newTask()]);
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
      await api.updateSchedule(id, s.id, { name: s.name, cron: s.cron, tasks: chainOf(s), enabled: !s.enabled });
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function remove(s: Schedule) {
    if (!window.confirm(t('Delete schedule "{name}"?', { name: s.name }))) return;
    try {
      await api.deleteSchedule(id, s.id);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <div className="card">
      <h3>{t("Scheduled tasks")}</h3>
      {list.length === 0 ? (
        <p className="muted">{t("No schedules yet.")}</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>{t("Name")}</th>
              <th>{t("Cron")}</th>
              <th>{t("Tasks")}</th>
              <th>{t("Next run")}</th>
              <th>{t("Last")}</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {list.map((s) => (
              <tr key={s.id}>
                <td>{s.name}</td>
                <td><code>{s.cron}</code></td>
                <td><TaskChain tasks={chainOf(s)} /></td>
                <td>{s.enabled ? fmt(s.nextRun) : "—"}</td>
                <td title={s.lastStatus}>{s.lastRun ? fmt(s.lastRun) : t("never")}</td>
                <td style={{ whiteSpace: "nowrap" }}>
                  <button onClick={() => toggle(s)}>{s.enabled ? t("Disable") : t("Enable")}</button>{" "}
                  <button className="danger" onClick={() => remove(s)}>{t("Delete")}</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <form onSubmit={add} style={{ marginTop: 12 }}>
        <div className="grid2">
          <div>
            <label>{t("Name")}</label>
            <input value={name} onChange={(e) => setName(e.target.value)} required placeholder={t("nightly restart")} />
          </div>
          <div>
            <label>{t("Cron (5 fields)")}</label>
            <input value={cron} onChange={(e) => setCron(e.target.value)} required placeholder="0 5 * * *" />
          </div>
        </div>

        <label style={{ marginTop: 8 }}>{t("Tasks (run in order)")}</label>
        {tasks.map((task, i) => (
          <div key={i} className="row" style={{ gap: 6, alignItems: "center", marginTop: 4, flexWrap: "wrap" }}>
            <span className="muted" style={{ width: 18 }}>{i + 1}.</span>
            <select value={task.action} onChange={(e) => patchTask(i, { action: e.target.value as ScheduleAction })} style={{ width: "auto" }}>
              {ACTIONS.map((a) => <option key={a} value={a}>{a}</option>)}
            </select>
            {task.action === "command" && (
              <input
                value={task.payload || ""}
                onChange={(e) => patchTask(i, { payload: e.target.value })}
                placeholder={t("say restarting soon")}
                style={{ flex: 1, minWidth: 160 }}
              />
            )}
            {i > 0 && (
              <label className="row" style={{ gap: 2, whiteSpace: "nowrap" }} title={t("Seconds to wait after the previous task")}>
                {t("wait")}
                <input
                  type="number"
                  min={0}
                  value={task.timeOffset}
                  onChange={(e) => patchTask(i, { timeOffset: Number(e.target.value) })}
                  style={{ width: 72 }}
                />
                s
              </label>
            )}
            <label className="row" style={{ gap: 2, whiteSpace: "nowrap" }} title={t("Keep going even if this task fails")}>
              <input type="checkbox" style={{ width: "auto" }} checked={!!task.continueOnFailure} onChange={(e) => patchTask(i, { continueOnFailure: e.target.checked })} />
              {t("continue on fail")}
            </label>
            {tasks.length > 1 && <button type="button" onClick={() => removeTask(i)}>✕</button>}
          </div>
        ))}
        <button type="button" onClick={addTask} style={{ marginTop: 6 }}>+ {t("Add task")}</button>

        {error && <div className="error" style={{ marginTop: 8 }}>{error}</div>}
        <div>
          <button className="primary" style={{ marginTop: 12 }} disabled={busy || !name || !cron}>
            {busy ? t("Adding…") : t("Add schedule")}
          </button>
        </div>
      </form>
    </div>
  );
}

// TaskChain renders a compact, ordered view of a schedule's tasks.
function TaskChain({ tasks }: { tasks: ScheduleTask[] }) {
  if (tasks.length === 0) return <span className="muted">—</span>;
  return (
    <span style={{ fontSize: 13 }}>
      {tasks.map((t, i) => (
        <span key={i}>
          {i > 0 && <span className="muted"> → </span>}
          {t.timeOffset > 0 && <span className="muted">+{t.timeOffset}s </span>}
          {t.action}
          {t.action === "command" && t.payload ? `: ${t.payload}` : ""}
          {t.continueOnFailure ? <span className="muted" title="continues on failure">*</span> : ""}
        </span>
      ))}
    </span>
  );
}

function fmt(iso?: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  return isNaN(d.getTime()) ? "—" : d.toLocaleString();
}
