import { useEffect, useState } from "react";
import { api, ApiError, CatalogEgg, Template } from "../api";
import { useT } from "../i18n";

// Templates is the admin egg catalog: import Pterodactyl/Pelican eggs, browse,
// edit (as native JSON), export and delete templates.
export function Templates() {
  const { t: tr } = useT();
  const [templates, setTemplates] = useState<Template[]>([]);
  const [error, setError] = useState("");
  const [msg, setMsg] = useState("");
  const [importJson, setImportJson] = useState("");
  const [importUrl, setImportUrl] = useState("");
  const [catalogUrl, setCatalogUrl] = useState("");
  const [catalog, setCatalog] = useState<CatalogEgg[]>([]);
  const [catalogErr, setCatalogErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [editing, setEditing] = useState<{ slug: string; json: string } | null>(null);

  async function load() {
    try {
      setTemplates(await api.templates());
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }
  async function loadCatalog() {
    try {
      const c = await api.eggCatalog();
      setCatalogUrl(c.catalogUrl);
      setCatalog(c.eggs);
      setCatalogErr(c.error ?? "");
    } catch (e) {
      setCatalogErr(e instanceof ApiError ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
    loadCatalog();
  }, []);

  async function importFrom(promise: Promise<Template>, ok: (t: Template) => string) {
    setBusy(true);
    setError("");
    setMsg("");
    try {
      setMsg(ok(await promise));
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  async function doImportUrl() {
    await importFrom(api.importEggUrl(importUrl.trim()), (t) => {
      setImportUrl("");
      return tr('Imported "{name}".', { name: t.name });
    });
  }

  async function saveCatalogUrl() {
    setError("");
    try {
      await api.setEggCatalog(catalogUrl.trim());
      await loadCatalog();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function doImport() {
    await importFrom(api.importEgg(importJson), (t) => {
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
    if (!window.confirm(tr('Delete template "{name}"? (Built-in ones return on restart.)', { name: t.name }))) return;
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
      <h2>{tr("Eggs / templates")}</h2>
      <p className="muted">{tr("The catalog of game/app templates. Import existing Pterodactyl/Pelican eggs, or edit and export your own.")}</p>

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
            style={{ width: "100%", minHeight: 280, fontFamily: "monospace" }}
          />
          <div className="row" style={{ marginTop: 8 }}>
            <button className="primary" onClick={saveEdit} disabled={busy}>{busy ? tr("Saving…") : tr("Save")}</button>
            <button onClick={() => setEditing(null)}>{tr("Cancel")}</button>
          </div>
        </div>
      ) : (
        <div style={{ marginTop: 12 }}>
          <h3>{tr("Import an egg")}</h3>
          <p className="muted">{tr("Paste a Pterodactyl/Pelican egg JSON. Importing one whose name matches an existing template updates it.")}</p>
          <textarea
            value={importJson}
            onChange={(e) => setImportJson(e.target.value)}
            spellCheck={false}
            placeholder='{ "name": "...", "docker_images": { ... }, "startup": "...", "variables": [ ... ] }'
            style={{ width: "100%", minHeight: 160, fontFamily: "monospace" }}
          />
          <button className="primary" style={{ marginTop: 8 }} onClick={doImport} disabled={busy || !importJson.trim()}>
            {busy ? tr("Importing…") : tr("Import egg")}
          </button>

          <h3 style={{ marginTop: 20 }}>{tr("Import from URL")}</h3>
          <p className="muted">{tr("Fetch an egg JSON straight from a URL (e.g. a raw file in an egg repository).")}</p>
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

          <h3 style={{ marginTop: 20 }}>{tr("Egg catalog")}</h3>
          <p className="muted">{tr("Point Quetzal at a catalog manifest (a JSON list of eggs) to browse and install community eggs in one click.")}</p>
          <div className="row">
            <input
              value={catalogUrl}
              onChange={(e) => setCatalogUrl(e.target.value)}
              placeholder={tr("Catalog manifest URL (https://…)")}
              style={{ flex: 1 }}
            />
            <button onClick={saveCatalogUrl} disabled={busy}>{tr("Save")}</button>
            <button onClick={loadCatalog} disabled={busy}>{tr("Refresh")}</button>
          </div>
          {catalogErr && <div className="error" style={{ marginTop: 8 }}>{catalogErr}</div>}
          {catalog.length > 0 && (
            <table style={{ marginTop: 8 }}>
              <thead><tr><th>{tr("Name")}</th><th>{tr("Category")}</th><th></th></tr></thead>
              <tbody>
                {catalog.map((e) => (
                  <tr key={e.url}>
                    <td>{e.name}{e.description ? <div className="muted" style={{ fontSize: 12 }}>{e.description}</div> : null}</td>
                    <td>{e.category || "—"}</td>
                    <td style={{ whiteSpace: "nowrap" }}>
                      <button className="primary" disabled={busy}
                        onClick={() => importFrom(api.importEggUrl(e.url), (t) => tr('Installed "{name}".', { name: t.name }))}>
                        {tr("Install")}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}

      {msg && <div className="notice">{msg}</div>}
      {error && <div className="error">{error}</div>}
    </div>
  );
}
