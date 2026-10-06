// What a clean reinstall will do to the top of a server's files, for the
// panel to show before anything is deleted. It reads the list of paths to
// keep the way the install step does: one shell pattern per line, matched
// from the root of the server's files, a leading dot matched only by a
// pattern that starts with one, as in sh.

export type KeepState = "kept" | "partly" | "deleted";

export interface KeepPreview {
  // Each top-level entry, with what happens to it: kept whole, kept only for
  // the paths inside it that are listed, or deleted.
  entries: { name: string; dir: boolean; state: KeepState }[];
  // The paths to keep that match nothing at the top, a mistyped world name
  // for instance.
  unmatched: string[];
}

function escapeRe(c: string): string {
  return /[.*+?^${}()|[\]\\/-]/.test(c) ? "\\" + c : c;
}

// segmentRe turns one segment of a shell pattern into a regular expression:
// * and ? within the segment, [...] and [!...] as sets, \ escaping.
function segmentRe(seg: string): RegExp {
  let re = "^";
  for (let i = 0; i < seg.length; i++) {
    const c = seg[i];
    if (c === "*") {
      re += ".*";
    } else if (c === "?") {
      re += ".";
    } else if (c === "[") {
      let k = i + 1;
      if (seg[k] === "!") k++;
      if (seg[k] === "]") k++; // a ] right after the opening is a member
      const end = seg.indexOf("]", k);
      if (end === -1) {
        re += "\\[";
        continue;
      }
      let body = seg.slice(i + 1, end);
      let neg = "";
      if (body.startsWith("!")) {
        neg = "^";
        body = body.slice(1);
      }
      re += "[" + neg + body.replace(/[\\\]^[]/g, (m) => "\\" + m) + "]";
      i = end;
    } else if (c === "\\" && i + 1 < seg.length) {
      re += escapeRe(seg[++i]);
    } else {
      re += escapeRe(c);
    }
  }
  return new RegExp(re + "$");
}

function matches(seg: string, name: string): boolean {
  if (name.startsWith(".") && !seg.startsWith(".")) return false;
  try {
    return segmentRe(seg).test(name);
  } catch {
    return false;
  }
}

// keepLines splits the text of the list into its paths, as the API reads them.
export function keepLines(text: string): string[] {
  return text
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l !== "");
}

export function keepPreview(entries: { name: string; dir: boolean }[], paths: string[]): KeepPreview {
  const state = new Map<string, KeepState>(entries.map((e) => [e.name, "deleted"]));
  const unmatched: string[] = [];
  for (const raw of paths) {
    const segs = raw.replace(/^(\.\/)+/, "").replace(/\/+$/, "").split("/").filter((s) => s !== "");
    if (segs.length === 0) continue;
    let hit = false;
    for (const e of entries) {
      if (!matches(segs[0], e.name)) continue;
      if (segs.length === 1) {
        state.set(e.name, "kept");
        hit = true;
      } else if (e.dir) {
        if (state.get(e.name) !== "kept") state.set(e.name, "partly");
        hit = true;
      }
    }
    if (!hit) unmatched.push(raw);
  }
  return {
    entries: entries.map((e) => ({ name: e.name, dir: e.dir, state: state.get(e.name) ?? "deleted" })),
    unmatched,
  };
}
