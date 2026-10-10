import { useEffect, useRef, useState } from "react";
import { api, hasAdminPerm, User, isAnyAdmin } from "../api";
import { LangSwitcher, useT } from "../i18n";
import { CreationRestriction, ServerList } from "./ServerList";
import { CreateServer } from "./CreateServer";
import { ServerDetail } from "./ServerDetail";
import { Admin } from "./Admin";
import { Account } from "./Account";
import { Lockup } from "./Brand";
import { ErrorBoundary } from "./ErrorBoundary";

type View =
  | { name: "list" }
  | { name: "create" }
  | { name: "detail"; id: number; tab?: string }
  | { name: "admin"; section?: string }
  | { name: "account" };

// The current view lives in the URL fragment (#/servers, #/servers/42,
// #/servers/42/files for one of its tabs, …) so a
// reload — or the browser's back/forward — restores the page instead of dropping
// the user back on the server list. parseHash/viewToHash are the single mapping.
function parseHash(): View {
  const parts = window.location.hash.replace(/^#\/?/, "").split("/").filter(Boolean);
  switch (parts[0]) {
    case "admin":
      return { name: "admin", section: parts[1] };
    case "account":
      return { name: "account" };
    case "servers":
      if (parts[1] === "new") return { name: "create" };
      if (parts[1] && /^\d+$/.test(parts[1])) return { name: "detail", id: Number(parts[1]), tab: parts[2] };
      return { name: "list" };
    default:
      return { name: "list" };
  }
}

function viewToHash(v: View): string {
  switch (v.name) {
    case "create":
      return "#/servers/new";
    case "detail":
      return `#/servers/${v.id}` + (v.tab ? `/${v.tab}` : "");
    case "admin":
      return "#/admin" + (v.section ? `/${v.section}` : "");
    case "account":
      return "#/account";
    default:
      return "#/servers";
  }
}

export function Dashboard({ user, onLogout, onUserRefresh }: { user: User; onLogout: () => void; onUserRefresh: () => Promise<void> }) {
  const [view, setView] = useState<View>(parseHash);
  const [unsaved, setUnsaved] = useState(false);
  const acceptedHash = useRef(window.location.hash);
  const { t } = useT();
  const canCreate = hasAdminPerm(user, "servers") || user.maxServers !== 0;

  // The hash is the source of truth: navigation writes it, and a hashchange
  // (our own writes, plus browser back/forward) drives the view state.
  useEffect(() => {
    const onHash = () => {
      const next = parseHash();
      const keepsDrafts = view.name === "detail" && next.name === "detail" && view.id === next.id;
      if (unsaved && !keepsDrafts && !window.confirm(t("Discard unsaved changes?"))) {
        // The browser has already moved to the new entry: push the kept address
        // on top instead of overwriting the one it moved to, which Back would lose.
        window.history.pushState(null, "", window.location.pathname + window.location.search + acceptedHash.current);
        return;
      }
      acceptedHash.current = window.location.hash;
      if (!keepsDrafts) setUnsaved(false);
      setView(next);
    };
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, [view, unsaved, t]);

  useEffect(() => {
    if (!unsaved) return;
    const warn = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [unsaved]);
  const go = (v: View) => {
    const h = viewToHash(v);
    if (window.location.hash === h) setView(v); // same hash: no hashchange fires
    else window.location.hash = h;
  };

  return (
    <>
      <div className="topbar">
        <div className="brand" onClick={() => go({ name: "list" })}>
          <Lockup />
        </div>
        <div className="row topnav">
          <button onClick={() => go({ name: "list" })}>{t("Servers")}</button>
          {isAnyAdmin(user) && <button onClick={() => go({ name: "admin" })}>{t("Admin")}</button>}
          <button onClick={() => go({ name: "account" })}>{t("Account")}</button>
          <span className="muted who">
            {user.username}
            {user.isAdmin ? ` ${t("(admin)")}` : isAnyAdmin(user) ? ` ${t("(scoped admin)")}` : ""}
          </span>
          <LangSwitcher />
          <button onClick={() => {
            if (!unsaved || window.confirm(t("Discard unsaved changes?"))) onLogout();
          }}>{t("Logout")}</button>
        </div>
      </div>
      <div className="container">
        {/* Keyed by the page, so going elsewhere gives it a fresh start: by
            the server, not its tab, which would remount the console. */}
        <ErrorBoundary
          key={view.name === "detail" ? `detail-${view.id}` : view.name}
          fallback={(err) => (
            <div className="card">
              <h3>{t("This page could not be shown")}</h3>
              <p className="muted">
                {t("Something on it failed to render. The rest of the panel still works; a bug report with the error below helps.")}
              </p>
              <pre className="error" style={{ whiteSpace: "pre-wrap" }}>{err.message}</pre>
              <button onClick={() => go({ name: "list" })}>{t("Back to the servers")}</button>
            </div>
          )}
        >
          {view.name === "list" && (
            <ServerList
              canCreate={canCreate}
              onUserRefresh={onUserRefresh}
              onCreate={() => go({ name: "create" })}
              onOpen={(id) => go({ name: "detail", id })}
            />
          )}
          {view.name === "create" && (canCreate ? (
            <CreateServer
              memoryRequired={!hasAdminPerm(user, "servers")}
              canImportTemplates={hasAdminPerm(user, "templates")}
              onDone={() => go({ name: "list" })}
              onCancel={() => go({ name: "list" })}
            />
          ) : (
            <div className="card">
              <h2>{t("New server")}</h2>
              <CreationRestriction onRefresh={onUserRefresh} />
              <button onClick={() => go({ name: "list" })}>{t("Back to the servers")}</button>
            </div>
          ))}
          {view.name === "detail" && (
            <ServerDetail key={view.id} id={view.id} tab={view.tab} user={user} onBack={() => go({ name: "list" })} onDirtyChange={setUnsaved} />
          )}
          {view.name === "admin" && (isAnyAdmin(user) ? <Admin user={user} section={view.section} /> : <ServerList canCreate={canCreate} onUserRefresh={onUserRefresh} onCreate={() => go({ name: "create" })} onOpen={(id) => go({ name: "detail", id })} />)}
          {view.name === "account" && <Account user={user} onUserRefresh={onUserRefresh} />}
        </ErrorBoundary>
      </div>
      <VersionFooter />
    </>
  );
}

// VersionFooter shows the running build version (from /api/version) so operators
// can tell what they're on at a glance.
function VersionFooter() {
  const [ver, setVer] = useState("");
  useEffect(() => {
    api.version().then((v) => setVer(v.version)).catch(() => {});
  }, []);
  if (!ver) return null;
  return (
    <div className="muted" style={{ textAlign: "center", padding: "16px 0", fontSize: 12 }}>
      Quetzal {ver}
    </div>
  );
}
