// Fails when a t("…") key in the components has no entry in a locale file.
// Coverage is easy to lose silently: a missing key falls back to the English
// source string, so an untranslated screen looks fine in development and only
// shows up to someone using the app in that language.
//
// The locale is evaluated rather than pattern-matched. Its keys are ordinary
// English sentences full of quotes, apostrophes and braces, and every regex
// that tries to read them gets some subset wrong — under-reporting duplicates
// or inventing missing keys that are already there.
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { runInNewContext } from "node:vm";

const webRoot = join(dirname(fileURLToPath(import.meta.url)), "..");
const srcRoot = join(webRoot, "src");

function walk(dir, out = []) {
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry);
    if (statSync(p).isDirectory()) walk(p, out);
    else if (/\.tsx?$/.test(p) && !p.includes("/locales/") && !p.endsWith("i18n.tsx")) out.push(p);
  }
  return out;
}

// Evaluated in an empty context rather than with eval: this runs during the
// build, over a file anyone can change in a pull request, and a locale is data,
// not code. The context has no require, no process and no fetch; dynamic import
// is refused because no import callback is supplied; and the timeout bounds a
// literal that tries to loop.
function localeKeys(file) {
  const src = readFileSync(file, "utf8");
  const body = src.slice(src.indexOf("{"), src.lastIndexOf("}") + 1);
  const dict = runInNewContext("(" + body + ")", Object.create(null), { timeout: 5000 });
  return new Set(Object.keys(dict));
}

// A template literal with a ${...} hole is a runtime-built key, not a literal
// one, so it can't be looked up here.
const call = /\bt\(\s*("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|`(?:[^`\\]|\\.)*`)/g;
const used = new Map();
for (const file of walk(srcRoot)) {
  for (const m of readFileSync(file, "utf8").matchAll(call)) {
    const raw = m[1];
    if (raw[0] === "`" && raw.includes("${")) continue;
    let key;
    try {
      key = runInNewContext("(" + raw + ")", Object.create(null), { timeout: 1000 });
    } catch {
      continue;
    }
    if (typeof key !== "string") continue;
    if (!used.has(key)) used.set(key, file.replace(srcRoot + "/", ""));
  }
}

let failed = false;
for (const locale of readdirSync(join(srcRoot, "locales")).filter((f) => /\.ts$/.test(f))) {
  const have = localeKeys(join(srcRoot, "locales", locale));
  const missing = [...used.keys()].filter((k) => !have.has(k)).sort();
  if (missing.length === 0) {
    console.log(`${locale}: ${used.size} keys, complete`);
    continue;
  }
  failed = true;
  console.error(`${locale}: ${missing.length} of ${used.size} keys untranslated`);
  for (const k of missing) console.error(`  ${JSON.stringify(k)}  (${used.get(k)})`);
}
process.exit(failed ? 1 : 0);
