import { useEffect, useState } from "react";
import { api, User, isAnyAdmin } from "../api";
import { LangSwitcher, useT } from "../i18n";
import { ServerList } from "./ServerList";
import { CreateServer } from "./CreateServer";
import { ServerDetail } from "./ServerDetail";
import { Admin } from "./Admin";
import { Account } from "./Account";

type View =
  | { name: "list" }
  | { name: "create" }
  | { name: "detail"; id: number }
  | { name: "admin" }
  | { name: "account" };

// The current view lives in the URL fragment (#/servers, #/servers/42, …) so a
// reload — or the browser's back/forward — restores the page instead of dropping
// the user back on the server list. parseHash/viewToHash are the single mapping.
function parseHash(): View {
  const parts = window.location.hash.replace(/^#\/?/, "").split("/").filter(Boolean);
  switch (parts[0]) {
    case "admin":
      return { name: "admin" };
    case "account":
      return { name: "account" };
    case "servers":
      if (parts[1] === "new") return { name: "create" };
      if (parts[1] && /^\d+$/.test(parts[1])) return { name: "detail", id: Number(parts[1]) };
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
      return `#/servers/${v.id}`;
    case "admin":
      return "#/admin";
    case "account":
      return "#/account";
    default:
      return "#/servers";
  }
}

export function Dashboard({ user, onLogout }: { user: User; onLogout: () => void }) {
  const [view, setView] = useState<View>(parseHash);
  const { t } = useT();

  // The hash is the source of truth: navigation writes it, and a hashchange
  // (our own writes, plus browser back/forward) drives the view state.
  useEffect(() => {
    const onHash = () => setView(parseHash());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);
  const go = (v: View) => {
    const h = viewToHash(v);
    if (window.location.hash === h) setView(v); // same hash: no hashchange fires
    else window.location.hash = h;
  };

  return (
    <>
      <div className="topbar">
        <div className="brand" style={{ cursor: "pointer" }} onClick={() => go({ name: "list" })}>
          Quetz<span>al</span>
        </div>
        <div className="row">
          <button onClick={() => go({ name: "list" })}>{t("Servers")}</button>
          {isAnyAdmin(user) && <button onClick={() => go({ name: "admin" })}>{t("Admin")}</button>}
          <button onClick={() => go({ name: "account" })}>{t("Account")}</button>
          <span className="muted">
            {user.username}
            {user.isAdmin ? ` ${t("(admin)")}` : isAnyAdmin(user) ? ` ${t("(scoped admin)")}` : ""}
          </span>
          <LangSwitcher />
          <button onClick={onLogout}>{t("Logout")}</button>
        </div>
      </div>
      <div className="container">
        {view.name === "list" && (
          <ServerList
            onCreate={() => go({ name: "create" })}
            onOpen={(id) => go({ name: "detail", id })}
          />
        )}
        {view.name === "create" && (
          <CreateServer
            onDone={() => go({ name: "list" })}
            onCancel={() => go({ name: "list" })}
          />
        )}
        {view.name === "detail" && (
          <ServerDetail id={view.id} user={user} onBack={() => go({ name: "list" })} />
        )}
        {view.name === "admin" && (isAnyAdmin(user) ? <Admin user={user} /> : <ServerList onCreate={() => go({ name: "create" })} onOpen={(id) => go({ name: "detail", id })} />)}
        {view.name === "account" && <Account user={user} />}
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
