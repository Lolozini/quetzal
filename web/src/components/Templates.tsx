import { useEffect, useState } from "react";
import { api, ApiError, ImportConflict, ImportIfExists, Template } from "../api";
import { useT } from "../i18n";
import { Collapsible } from "./Collapsible";

// Templates is the admin egg manager: import Pterodactyl/Pelican eggs (pasted or
// by URL) or a template exported from another install, browse, edit (as native
// JSON), export and delete templates.
export function Templates() {
  const { t: tr } = useT();
  const [templates, setTemplates] = useState<Template[]>([]);
  const [error, setError] = useState("");
  const [msg, setMsg] = useState("");
  const [importJson, setImportJson] = useState("");
  const [importUrl, setImportUrl] = useState("");
  const [busy, setBusy] = useState(false);
  const [editing, setEditing] = useState<{ slug: string; json: string } | null>(null);
  // An import refused because its slug is taken: what the panel said, and the
  // same import again with the admin's answer.
  const [conflict, setConflict] = useState<{ message: string; retry: (ifExists: ImportIfExists) => void } | null>(null);

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

  // importFrom runs an import. One whose slug another template has is refused
  // rather than replacing it, a different egg sharing its name included: the
  // admin is asked whether to replace that template or to add this one.
  async function importFrom(run: (ifExists?: ImportIfExists) => Promise<Template>, ok: (t: Template) => string, ifExists?: ImportIfExists) {
    setBusy(true);
    setError("");
    setMsg("");
    setConflict(null);
    try {
      setMsg(ok(await run(ifExists)));
      await load();
    } catch (e) {
      const taken = e instanceof ApiError && e.status === 409 && !ifExists ? (e.data as ImportConflict | undefined)?.existing : undefined;
      if (taken) {
        setConflict({
          message: tr('A template "{name}" ({slug}, version {version}) already exists, used by {servers} server(s).', {
            name: taken.name, slug: taken.slug, version: taken.version, servers: taken.servers,
          }),
          retry: (mode) => importFrom(run, ok, mode),
        });
      } else {
        setError(e instanceof ApiError ? e.message : String(e));
      }
    } finally {
      setBusy(false);
    }
  }

  async function doImportUrl() {
    const url = importUrl.trim();
    await importFrom((ifExists) => api.importEggUrl(url, ifExists), (t) => {
      setImportUrl("");
      return tr('Imported "{name}".', { name: t.name });
    });
  }

  async function doImport() {
    const json = importJson;
    await importFrom((ifExists) => api.importEgg(json, ifExists), (t) => {
      setImportJson("");
      return tr('Imported "{name}".', { name: t.name });
    });
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
      setMsg(tr("Template saved."));
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  async function remove(t: Template) {
    if (!window.confirm(tr('Delete template "{name}"?', { name: t.name }))) return;
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
      <Collapsible title={tr("Eggs / templates")} count={templates.length}>
      <p className="muted">{tr("The game/app templates available to servers. Import existing Pterodactyl/Pelican eggs, or edit and export your own.")}</p>

      {templates.length > 0 && (
        <table>
          <thead>
            <tr><th>{tr("Name")}</th><th>{tr("Slug")}</th><th>{tr("Images")}</th><th>{tr("Vars")}</th><th>{tr("Ver")}</th><th></th></tr>
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
                  <button onClick={() => openEdit(t.slug)}>{tr("Edit")}</button>{" "}
                  <button type="button" onClick={() => { window.location.href = api.templateExportUrl(t.slug); }}>{tr("Export")}</button>{" "}
                  <button className="danger" onClick={() => remove(t)}>{tr("Delete")}</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {editing ? (
        <div style={{ marginTop: 12 }}>
          <h3>{tr("Edit {slug}", { slug: editing.slug })}</h3>
          <p className="muted">{tr("Native template JSON. The slug is fixed; changes bump the version and restart affected servers on the next reconcile.")}</p>
          <textarea
            value={editing.json}
            onChange={(e) => setEditing({ ...editing, json: e.target.value })}
            spellCheck={false}
            style={{ width: "100%", minHeight: 280, fontFamily: "var(--font-mono)" }}
          />
          <div className="row" style={{ marginTop: 8 }}>
            <button className="primary" onClick={saveEdit} disabled={busy}>{busy ? tr("Saving…") : tr("Save")}</button>
            <button onClick={() => setEditing(null)}>{tr("Cancel")}</button>
          </div>
          {msg && <div className="notice" style={{ marginTop: 8 }}>{msg}</div>}
          {error && <div className="error" style={{ marginTop: 8 }}>{error}</div>}
        </div>
      ) : (
        <div style={{ marginTop: 12 }}>
          <h3>{tr("Import an egg")}</h3>
          <p className="muted">{tr("Paste a Pterodactyl/Pelican egg, or a template exported from another install. If a template already has its name, you choose whether to replace it or to add this one beside it.")}</p>
          <textarea
            value={importJson}
            onChange={(e) => setImportJson(e.target.value)}
            spellCheck={false}
            placeholder='{ "name": "...", "docker_images": { ... }, "startup": "...", "variables": [ ... ] }'
            style={{ width: "100%", minHeight: 160, fontFamily: "var(--font-mono)" }}
          />
          <button className="primary" style={{ marginTop: 8 }} onClick={doImport} disabled={busy || !importJson.trim()}>
            {busy ? tr("Importing…") : tr("Import egg")}
          </button>

          <h3 style={{ marginTop: 20 }}>{tr("Import from URL")}</h3>
          <p className="muted">{tr("Fetch an egg straight from a URL, e.g. a file in github.com/pelican-eggs (Pterodactyl JSON and Pelican YAML are both read).")}</p>
          <div className="row">
            <input
              value={importUrl}
              onChange={(e) => setImportUrl(e.target.value)}
              placeholder="https://…/egg.json"
              style={{ flex: 1 }}
            />
            <button className="primary" onClick={doImportUrl} disabled={busy || !importUrl.trim()}>
              {tr("Import")}
            </button>
          </div>
          <p className="muted" style={{ fontSize: 12, marginTop: 4 }}>
            {tr("Point at the egg file; a GitHub/GitLab file page link is converted to the raw file automatically.")}
          </p>

          {/* Feedback sits with the import controls: this panel scrolls, so a
              message at the bottom of the card would be missed. */}
          {conflict && (
            <div className="notice warn" style={{ marginTop: 8 }}>
              <div>{conflict.message}</div>
              <div className="row" style={{ marginTop: 8 }}>
                <button className="danger" disabled={busy} onClick={() => conflict.retry("replace")}>{tr("Replace it")}</button>
                <button disabled={busy} onClick={() => conflict.retry("copy")}>{tr("Import as a new template")}</button>
                <button disabled={busy} onClick={() => setConflict(null)}>{tr("Cancel")}</button>
              </div>
            </div>
          )}
          {msg && <div className="notice" style={{ marginTop: 8 }}>{msg}</div>}
          {error && <div className="error" style={{ marginTop: 8 }}>{error}</div>}
        </div>
      )}

      </Collapsible>
    </div>
  );
}
