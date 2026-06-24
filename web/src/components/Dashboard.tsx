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

export function Dashboard({ user, onLogout }: { user: User; onLogout: () => void }) {
  const [view, setView] = useState<View>({ name: "list" });
  const { t } = useT();

  return (
    <>
      <div className="topbar">
        <div className="brand" style={{ cursor: "pointer" }} onClick={() => setView({ name: "list" })}>
          Quetz<span>al</span>
        </div>
        <div className="row">
          <button onClick={() => setView({ name: "list" })}>{t("Servers")}</button>
          {isAnyAdmin(user) && <button onClick={() => setView({ name: "admin" })}>{t("Admin")}</button>}
          <button onClick={() => setView({ name: "account" })}>{t("Account")}</button>
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
            onCreate={() => setView({ name: "create" })}
            onOpen={(id) => setView({ name: "detail", id })}
          />
        )}
        {view.name === "create" && (
          <CreateServer
            onDone={() => setView({ name: "list" })}
            onCancel={() => setView({ name: "list" })}
          />
        )}
        {view.name === "detail" && (
          <ServerDetail id={view.id} user={user} onBack={() => setView({ name: "list" })} />
        )}
        {view.name === "admin" && isAnyAdmin(user) && <Admin user={user} />}
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
