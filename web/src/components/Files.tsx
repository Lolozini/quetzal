import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError, FileEntry } from "../api";

const EDIT_MAX = 1 << 20; // 1 MiB: larger files are download-only

// join builds a relative path under the data root from a dir and a name.
function join(dir: string, name: string): string {
  return dir ? `${dir}/${name}` : name;
}
function parent(dir: string): string {
  const i = dir.lastIndexOf("/");
  return i < 0 ? "" : dir.slice(0, i);
}

export function Files({ id }: { id: number }) {
  const [path, setPath] = useState(""); // relative to the data root
  const [entries, setEntries] = useState<FileEntry[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [editing, setEditing] = useState<{ path: string; content: string } | null>(null);
  const [saved, setSaved] = useState("");
  const uploadRef = useRef<HTMLInputElement>(null);

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

  async function open(e: FileEntry) {
    if (e.dir) {
      setEditing(null);
      setPath(join(path, e.name));
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
      setSaved("Saved.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function newFolder() {
    const name = window.prompt("New folder name:");
    if (!name) return;
    try {
      await api.mkdir(id, join(path, name));
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  async function rename(e: FileEntry) {
    const to = window.prompt(`Rename "${e.name}" to:`, e.name);
    if (!to || to === e.name) return;
    try {
      await api.renameFile(id, join(path, e.name), join(path, to));
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  async function remove(e: FileEntry) {
    if (!window.confirm(`Delete "${e.name}"${e.dir ? " and everything inside it" : ""}?`)) return;
    try {
      await api.deleteFile(id, join(path, e.name));
      if (editing && editing.path.startsWith(join(path, e.name))) setEditing(null);
      await load();
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
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="card">
      <h2>Files</h2>

      <div className="row" style={{ gap: 8, flexWrap: "wrap", alignItems: "center" }}>
        <button disabled={path === ""} onClick={() => { setEditing(null); setPath(parent(path)); }}>↑ Up</button>
        <code>/{path}</code>
        <span style={{ flex: 1 }} />
        <button onClick={load} disabled={busy}>Refresh</button>
        <button onClick={newFolder}>New folder</button>
        <button onClick={() => uploadRef.current?.click()}>Upload</button>
        <input ref={uploadRef} type="file" style={{ display: "none" }} onChange={upload} />
      </div>

      {error && <div className="error" style={{ marginTop: 8 }}>{error}</div>}

      <table style={{ marginTop: 8 }}>
        <thead>
          <tr><th>Name</th><th>Size</th><th></th></tr>
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
                  {!e.dir && <a href={api.fileDownloadUrl(id, join(path, e.name))}>Download</a>}{" "}
                  <button onClick={() => rename(e)}>Rename</button>{" "}
                  <button className="danger" onClick={() => remove(e)}>Delete</button>
                </td>
              </tr>
            ))}
          {entries.length === 0 && !error && (
            <tr><td colSpan={3} className="muted">Empty directory.</td></tr>
          )}
        </tbody>
      </table>

      {editing && (
        <div style={{ marginTop: 12 }}>
          <h3>Editing <code>/{editing.path}</code></h3>
          <textarea
            value={editing.content}
            onChange={(e) => { setEditing({ ...editing, content: e.target.value }); setSaved(""); }}
            spellCheck={false}
            style={{ width: "100%", minHeight: 320, fontFamily: "monospace" }}
          />
          <div className="row" style={{ marginTop: 8 }}>
            <button className="primary" onClick={save} disabled={busy}>Save</button>
            <button onClick={() => setEditing(null)}>Close</button>
            {saved && <span className="notice">{saved}</span>}
          </div>
        </div>
      )}
    </div>
  );
}

function humanSize(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(1)} ${units[i]}`;
}
