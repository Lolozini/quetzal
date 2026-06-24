import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError, FileEntry } from "../api";
import { useT } from "../i18n";

const EDIT_MAX = 1 << 20; // 1 MiB: larger files are download-only

function join(dir: string, name: string): string {
  return dir ? `${dir}/${name}` : name;
}

export function Files({ id }: { id: number }) {
  const { t } = useT();
  const [path, setPath] = useState(""); // relative to the data root
  const [entries, setEntries] = useState<FileEntry[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [editing, setEditing] = useState<{ path: string; content: string } | null>(null);
  const [saved, setSaved] = useState("");
  const [mut, setMut] = useState(0); // bumped on changes so the tree refreshes
  const uploadRef = useRef<HTMLInputElement>(null);
  const archiveRef = useRef<HTMLInputElement>(null);

  const load = useCallback(async () => {
    setError("");
    setBusy(true);
    try {
      setEntries(await api.listFiles(id, path));
    } catch (e) {
      setEntries([]);
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }, [id, path]);

  useEffect(() => {
    load();
  }, [load]);

  function nav(p: string) {
    setEditing(null);
    setPath(p);
  }
  function changed() {
    setMut((n) => n + 1);
    load();
  }

  async function open(e: FileEntry) {
    if (e.dir) {
      nav(join(path, e.name));
      return;
    }
    const p = join(path, e.name);
    if (e.size > EDIT_MAX) {
      window.location.href = api.fileDownloadUrl(id, p);
      return;
    }
    setError("");
    try {
      setEditing({ path: p, content: await api.readFile(id, p) });
      setSaved("");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  async function save() {
    if (!editing) return;
    setBusy(true);
    setError("");
    try {
      await api.writeFile(id, editing.path, editing.content);
      setSaved(t("Saved."));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function newFolder() {
    const name = window.prompt(t("New folder name:"));
    if (!name) return;
    try {
      await api.mkdir(id, join(path, name));
      changed();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function rename(e: FileEntry) {
    const to = window.prompt(t('Rename "{name}" to:', { name: e.name }), e.name);
    if (!to || to === e.name) return;
    try {
      await api.renameFile(id, join(path, e.name), join(path, to));
      changed();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  async function remove(e: FileEntry) {
    if (!window.confirm(e.dir ? t('Delete "{name}" and everything inside it?', { name: e.name }) : t('Delete "{name}"?', { name: e.name }))) return;
    try {
      await api.deleteFile(id, join(path, e.name));
      if (editing && editing.path.startsWith(join(path, e.name))) setEditing(null);
      changed();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  async function upload(ev: React.ChangeEvent<HTMLInputElement>) {
    const file = ev.target.files?.[0];
    ev.target.value = "";
    if (!file) return;
    setBusy(true);
    setError("");
    try {
      await api.writeFile(id, join(path, file.name), file);
      changed();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function uploadArchive(ev: React.ChangeEvent<HTMLInputElement>) {
    const file = ev.target.files?.[0];
    ev.target.value = "";
    if (!file) return;
    const format = /\.zip$/i.test(file.name) ? "zip" : "tar";
    if (!window.confirm(t('Extract "{file}" into /{path}? Existing files with the same names are overwritten.', { file: file.name, path: path || "" }))) return;
    setBusy(true);
    setError("");
    try {
      await api.extractArchive(id, path, format, file);
      changed();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  const segs = path ? path.split("/") : [];

  return (
    <div className="card">
      <h2>{t("Files")}</h2>

      {/* Breadcrumb */}
      <div className="row" style={{ gap: 4, flexWrap: "wrap", alignItems: "center" }}>
        <a href="#" onClick={(e) => { e.preventDefault(); nav(""); }}>{t("root")}</a>
        {segs.map((seg, i) => {
          const p = segs.slice(0, i + 1).join("/");
          return (
            <span key={p}>
              <span className="muted"> / </span>
              <a href="#" onClick={(e) => { e.preventDefault(); nav(p); }}>{seg}</a>
            </span>
          );
        })}
        <span style={{ flex: 1 }} />
        <button onClick={load} disabled={busy}>{t("Refresh")}</button>
        <button onClick={newFolder}>{t("New folder")}</button>
        <button onClick={() => uploadRef.current?.click()}>{t("Upload")}</button>
        <button onClick={() => archiveRef.current?.click()} disabled={busy}>{t("Upload archive")}</button>
        <a href={api.fileArchiveUrl(id, path)}><button type="button">{t("Download folder")}</button></a>
        <input ref={uploadRef} type="file" style={{ display: "none" }} onChange={upload} />
        <input ref={archiveRef} type="file" accept=".zip,.tar,.gz,.tgz,.bz2,.xz" style={{ display: "none" }} onChange={uploadArchive} />
      </div>

      {error && <div className="error" style={{ marginTop: 8 }}>{error}</div>}

      <div className="row" style={{ alignItems: "flex-start", gap: 12, marginTop: 8 }}>
        {/* Tree sidebar */}
        <div style={{ width: 240, minWidth: 200, maxHeight: 420, overflow: "auto", borderRight: "1px solid var(--border, #333)", paddingRight: 8 }}>
          <DirTree id={id} current={path} onNavigate={nav} reload={mut} />
        </div>

        {/* Current directory */}
        <div style={{ flex: 1, minWidth: 0 }}>
          <table>
            <thead>
              <tr><th>{t("Name")}</th><th>{t("Size")}</th><th></th></tr>
            </thead>
            <tbody>
              {entries
                .slice()
                .sort((a, b) => (a.dir === b.dir ? a.name.localeCompare(b.name) : a.dir ? -1 : 1))
                .map((e) => (
                  <tr key={e.name}>
                    <td>
                      <a href="#" onClick={(ev) => { ev.preventDefault(); open(e); }}>
                        {e.dir ? "📁 " : "📄 "}{e.name}
                      </a>
                    </td>
                    <td>{e.dir ? "" : humanSize(e.size)}</td>
                    <td style={{ whiteSpace: "nowrap" }}>
                      <a href={e.dir ? api.fileArchiveUrl(id, join(path, e.name)) : api.fileDownloadUrl(id, join(path, e.name))}>{t("Download")}</a>{" "}
                      <button onClick={() => rename(e)}>{t("Rename")}</button>{" "}
                      <button className="danger" onClick={() => remove(e)}>{t("Delete")}</button>
                    </td>
                  </tr>
                ))}
              {entries.length === 0 && !error && (
                <tr><td colSpan={3} className="muted">{t("Empty directory.")}</td></tr>
              )}
            </tbody>
          </table>

          {editing && (
            <div style={{ marginTop: 12 }}>
              <h3>{t("Editing")} <code>/{editing.path}</code></h3>
              <textarea
                value={editing.content}
                onChange={(e) => { setEditing({ ...editing, content: e.target.value }); setSaved(""); }}
                spellCheck={false}
                style={{ width: "100%", minHeight: 320, fontFamily: "monospace" }}
              />
              <div className="row" style={{ marginTop: 8 }}>
                <button className="primary" onClick={save} disabled={busy}>{t("Save")}</button>
                <button onClick={() => setEditing(null)}>{t("Close")}</button>
                {saved && <span className="notice">{saved}</span>}
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

// DirTree is a lazily-loaded folder tree. Directories expand on click and their
// children are fetched on demand; navigating selects the folder in the main pane.
function DirTree({
  id, current, onNavigate, reload,
}: {
  id: number;
  current: string;
  onNavigate: (p: string) => void;
  reload: number;
}) {
  const { t } = useT();
  const [open, setOpen] = useState<Set<string>>(() => new Set([""]));
  const [kids, setKids] = useState<Record<string, FileEntry[]>>({});

  const loadKids = useCallback(async (p: string) => {
    try {
      const all = await api.listFiles(id, p);
      setKids((prev) => ({ ...prev, [p]: all.filter((e) => e.dir) }));
    } catch {
      setKids((prev) => ({ ...prev, [p]: [] }));
    }
  }, [id]);

  // (Re)load the root and any open folders when the tree or data changes.
  useEffect(() => {
    setOpen((cur) => {
      cur.forEach((p) => loadKids(p));
      return cur;
    });
  }, [reload, loadKids]);

  // Expand and load the ancestors of the current path so the selection is shown.
  useEffect(() => {
    const anc = [""];
    let acc = "";
    for (const seg of current ? current.split("/") : []) {
      acc = acc ? `${acc}/${seg}` : seg;
      anc.push(acc);
    }
    setOpen((prev) => {
      const n = new Set(prev);
      anc.forEach((p) => n.add(p));
      return n;
    });
    anc.forEach((p) => loadKids(p));
  }, [current, loadKids]);

  function toggle(p: string) {
    setOpen((prev) => {
      const n = new Set(prev);
      if (n.has(p)) {
        n.delete(p);
      } else {
        n.add(p);
        loadKids(p);
      }
      return n;
    });
  }

  function node(p: string, name: string, depth: number) {
    const isOpen = open.has(p);
    return (
      <div key={p || "/"}>
        <div
          className="row"
          style={{ gap: 2, paddingLeft: depth * 12, cursor: "pointer", fontWeight: p === current ? 600 : 400 }}
        >
          <span onClick={() => toggle(p)} style={{ width: 14, display: "inline-block", textAlign: "center" }}>
            {isOpen ? "▾" : "▸"}
          </span>
          <span onClick={() => { onNavigate(p); if (!isOpen) toggle(p); }} style={{ whiteSpace: "nowrap" }}>
            📁 {name || t("root")}
          </span>
        </div>
        {isOpen && (kids[p] || []).map((c) => node(p ? `${p}/${c.name}` : c.name, c.name, depth + 1))}
      </div>
    );
  }

  return <div style={{ fontSize: 14 }}>{node("", "", 0)}</div>;
}

function humanSize(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(1)} ${units[i]}`;
}
