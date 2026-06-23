import { useEffect, useState } from "react";
import { api, ApiError, Template } from "../api";

// Templates is the admin egg catalog: import Pterodactyl/Pelican eggs, browse,
// edit (as native JSON), export and delete templates.
export function Templates() {
  const [templates, setTemplates] = useState<Template[]>([]);
  const [error, setError] = useState("");
  const [msg, setMsg] = useState("");
  const [importJson, setImportJson] = useState("");
  const [busy, setBusy] = useState(false);
  const [editing, setEditing] = useState<{ slug: string; json: string } | null>(null);

  async function load() {
    try {
      setTemplates(await api.templates());
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, []);

  async function doImport() {
    setBusy(true);
    setError("");
    setMsg("");
    try {
      const t = await api.importEgg(importJson);
      setImportJson("");
      setMsg(`Imported "${t.name}".`);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  async function openEdit(slug: string) {
    setError("");
    try {
      const t = await api.template(slug);
      setEditing({ slug, json: JSON.stringify(t, null, 2) });
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function saveEdit() {
    if (!editing) return;
    setBusy(true);
    setError("");
    setMsg("");
    try {
      await api.updateTemplate(editing.slug, editing.json);
      setEditing(null);
      setMsg("Template saved.");
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  async function remove(t: Template) {
    if (!window.confirm(`Delete template "${t.name}"? (Built-in ones return on restart.)`)) return;
    setError("");
    try {
      await api.deleteTemplate(t.slug);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <div className="card">
      <h2>Eggs / templates</h2>
      <p className="muted">The catalog of game/app templates. Import existing Pterodactyl/Pelican eggs, or edit and export your own.</p>

      {templates.length > 0 && (
        <table>
          <thead>
            <tr><th>Name</th><th>Slug</th><th>Images</th><th>Vars</th><th>Ver</th><th></th></tr>
          </thead>
          <tbody>
            {templates.map((t) => (
              <tr key={t.id}>
                <td>{t.name}</td>
                <td><code>{t.slug}</code></td>
                <td>{t.images?.length ?? 0}</td>
                <td>{t.variables?.length ?? 0}</td>
                <td>{t.version ?? "—"}</td>
                <td style={{ whiteSpace: "nowrap" }}>
                  <button onClick={() => openEdit(t.slug)}>Edit</button>{" "}
                  <a href={api.templateExportUrl(t.slug)}><button type="button">Export</button></a>{" "}
                  <button className="danger" onClick={() => remove(t)}>Delete</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {editing ? (
        <div style={{ marginTop: 12 }}>
          <h3>Edit {editing.slug}</h3>
          <p className="muted">Native template JSON. The slug is fixed; changes bump the version and restart affected servers on the next reconcile.</p>
          <textarea
            value={editing.json}
            onChange={(e) => setEditing({ ...editing, json: e.target.value })}
            spellCheck={false}
            style={{ width: "100%", minHeight: 280, fontFamily: "monospace" }}
          />
          <div className="row" style={{ marginTop: 8 }}>
            <button className="primary" onClick={saveEdit} disabled={busy}>{busy ? "Saving…" : "Save"}</button>
            <button onClick={() => setEditing(null)}>Cancel</button>
          </div>
        </div>
      ) : (
        <div style={{ marginTop: 12 }}>
          <h3>Import an egg</h3>
          <p className="muted">Paste a Pterodactyl/Pelican egg JSON. Importing one whose name matches an existing template updates it.</p>
          <textarea
            value={importJson}
            onChange={(e) => setImportJson(e.target.value)}
            spellCheck={false}
            placeholder='{ "name": "...", "docker_images": { ... }, "startup": "...", "variables": [ ... ] }'
            style={{ width: "100%", minHeight: 160, fontFamily: "monospace" }}
          />
          <button className="primary" style={{ marginTop: 8 }} onClick={doImport} disabled={busy || !importJson.trim()}>
            {busy ? "Importing…" : "Import egg"}
          </button>
        </div>
      )}

      {msg && <div className="notice">{msg}</div>}
      {error && <div className="error">{error}</div>}
    </div>
  );
}
