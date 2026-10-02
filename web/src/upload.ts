import { api, ApiError } from "./api";

// Sending a file in pieces. One request per file lasted as long as the
// transfer, and the proxy in front of the panel gives a request a fixed time to
// arrive (Traefik 3: 60 seconds), so a large file on a home connection was cut
// and lost whole. Each piece here is sized to take a few seconds, from how
// fast the last one went; one that fails is sent again from where the server
// says the upload is, and choosing the same file again later resumes it.

// A piece is sized to take about this long.
const TARGET_SECONDS = 8;
const MIN_PIECE = 256 << 10;
const MAX_PIECE = 16 << 20;
const FIRST_PIECE = 1 << 20;
const RETRIES = 6;

export interface UploadTarget {
  path: string; // the file, or for an archive the directory it unpacks into
  kind: "file" | "archive";
  format?: "zip" | "tar";
}

export interface UploadProgress {
  uploadId: string; // for cancelUpload
  sent: number;
  total: number;
  resumed: boolean;
}

// Which unfinished upload holds which local file. The server knows an upload's
// destination and size, not which file it came from, so a different file of
// the same name and size must not be taken for it.
const RESUME_KEY = "quetzal.uploads";

function fingerprint(serverId: number, file: File, t: UploadTarget): string {
  return [serverId, t.kind, t.format ?? "", t.path, file.name, file.size, file.lastModified].join("\u0000");
}

function loadResume(): Record<string, string> {
  try {
    return JSON.parse(localStorage.getItem(RESUME_KEY) ?? "{}") as Record<string, string>;
  } catch {
    return {};
  }
}

function saveResume(m: Record<string, string>) {
  try {
    localStorage.setItem(RESUME_KEY, JSON.stringify(m));
  } catch {
    /* private window: no resuming across reloads, nothing else lost */
  }
}

const sleep = (ms: number, signal: AbortSignal) =>
  new Promise<void>((resolve, reject) => {
    const t = setTimeout(resolve, ms);
    signal.addEventListener("abort", () => { clearTimeout(t); reject(new DOMException("aborted", "AbortError")); }, { once: true });
  });

// sendInPieces uploads file to a server and finishes it there. Aborting the
// signal stops sending; the upload then stays on the server, to be resumed or
// cancelled (cancelUpload).
export async function sendInPieces(
  serverId: number,
  file: File,
  target: UploadTarget,
  onProgress: (p: UploadProgress) => void,
  signal: AbortSignal,
): Promise<void> {
  const key = fingerprint(serverId, file, target);
  const resume = loadResume();
  let uid = "";
  let offset = 0;
  let chunkMax = MAX_PIECE;
  let resumed = false;

  const known = resume[key];
  if (known) {
    try {
      const u = await api.upload(serverId, known);
      uid = u.id;
      offset = u.received;
      chunkMax = u.chunkMax;
      resumed = offset > 0;
    } catch {
      delete resume[key]; // expired or finished elsewhere: start again
    }
  }
  if (!uid) {
    const u = await api.startUpload(serverId, { ...target, size: file.size });
    uid = u.id;
    chunkMax = u.chunkMax;
    resume[key] = uid;
    saveResume(resume);
  }
  const cap = Math.min(MAX_PIECE, chunkMax || MAX_PIECE);
  let size = FIRST_PIECE;
  let failures = 0;
  onProgress({ uploadId: uid, sent: offset, total: file.size, resumed });

  for (;;) {
    while (offset < file.size) {
      const end = Math.min(file.size, offset + size);
      const started = performance.now();
      try {
        const res = await api.uploadPiece(serverId, uid, offset, file.slice(offset, end), signal);
        const seconds = Math.max((performance.now() - started) / 1000, 0.05);
        const rate = (res.received - offset) / seconds;
        offset = res.received;
        failures = 0;
        if (rate > 0) size = Math.max(MIN_PIECE, Math.min(cap, Math.round(rate * TARGET_SECONDS)));
      } catch (e) {
        if (signal.aborted) throw e;
        const where = e instanceof ApiError ? (e.data as { received?: number } | undefined)?.received : undefined;
        if (e instanceof ApiError && e.status === 409 && typeof where === "number") {
          offset = where; // the server says where the upload is: go on from there
          continue;
        }
        if (e instanceof ApiError && e.status >= 400 && e.status < 500 && e.status !== 409 && e.status !== 408 && e.status !== 429) {
          throw e; // refused, not interrupted: sending again would not help
        }
        if (++failures > RETRIES) throw e;
        size = Math.max(MIN_PIECE, Math.round(size / 2)); // a cut often means too slow
        await sleep(1000 * 2 ** (failures - 1), signal);
        try {
          offset = (await api.upload(serverId, uid)).received;
        } catch {
          /* try the piece again as it was */
        }
      }
      onProgress({ uploadId: uid, sent: offset, total: file.size, resumed });
    }
    try {
      await api.completeUpload(serverId, uid);
      break;
    } catch (e) {
      // Not all there after all (a piece the server lost): fetch where it is
      // and go on.
      if (e instanceof ApiError && e.status === 409 && failures++ < RETRIES) {
        offset = (await api.upload(serverId, uid)).received;
        continue;
      }
      throw e;
    }
  }
  const after = loadResume();
  delete after[key];
  saveResume(after);
}

// forgetUpload drops a cancelled upload from the resume list.
export function forgetUpload(uid: string) {
  const m = loadResume();
  for (const k of Object.keys(m)) if (m[k] === uid) delete m[k];
  saveResume(m);
}
