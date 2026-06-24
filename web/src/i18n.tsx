// Lightweight, dependency-free i18n. Translation keys ARE the English source
// strings, so English always renders (even for not-yet-translated keys) and
// other locales are just override dictionaries (English → translation). Adding
// a language is a single dictionary file; adding coverage is wrapping a string
// in t(). This mirrors how Pterodactyl grows translations by community
// contribution.
import { createContext, ReactNode, useCallback, useContext, useState } from "react";
import { fr } from "./locales/fr";

export type Lang = "en" | "fr";

// Per-locale override dictionaries. English is the identity (keys are English),
// so it needs no dictionary.
const DICTS: Partial<Record<Lang, Record<string, string>>> = { fr };

export const LANGS: { code: Lang; label: string }[] = [
  { code: "en", label: "English" },
  { code: "fr", label: "Français" },
];

const STORAGE_KEY = "quetzal.lang";

function detect(): Lang {
  const saved = localStorage.getItem(STORAGE_KEY);
  if (saved === "en" || saved === "fr") return saved;
  return navigator.language?.toLowerCase().startsWith("fr") ? "fr" : "en";
}

export type TFunc = (key: string, vars?: Record<string, string | number>) => string;

interface Ctx {
  lang: Lang;
  setLang: (l: Lang) => void;
  t: TFunc;
}

const LocaleContext = createContext<Ctx | null>(null);

export function LocaleProvider({ children }: { children: ReactNode }) {
  const [lang, setLangState] = useState<Lang>(detect);

  const setLang = useCallback((l: Lang) => {
    localStorage.setItem(STORAGE_KEY, l);
    setLangState(l);
  }, []);

  const t = useCallback<TFunc>(
    (key, vars) => {
      let s = DICTS[lang]?.[key] ?? key;
      if (vars) {
        for (const k of Object.keys(vars)) {
          s = s.replace(new RegExp(`\\{${k}\\}`, "g"), String(vars[k]));
        }
      }
      return s;
    },
    [lang],
  );

  return <LocaleContext.Provider value={{ lang, setLang, t }}>{children}</LocaleContext.Provider>;
}

export function useT(): Ctx {
  const ctx = useContext(LocaleContext);
  if (!ctx) throw new Error("useT must be used within a LocaleProvider");
  return ctx;
}

// LangSwitcher is a compact locale picker (used in the top bar and on the login
// screen).
export function LangSwitcher() {
  const { lang, setLang } = useT();
  return (
    <select
      aria-label="Language"
      value={lang}
      onChange={(e) => setLang(e.target.value as Lang)}
      style={{ width: "auto" }}
    >
      {LANGS.map((l) => (
        <option key={l.code} value={l.code}>{l.label}</option>
      ))}
    </select>
  );
}
