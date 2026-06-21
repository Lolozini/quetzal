import { useState } from "react";
import { User } from "../api";
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

  return (
    <>
      <div className="topbar">
        <div className="brand" style={{ cursor: "pointer" }} onClick={() => setView({ name: "list" })}>
          Quetz<span>al</span>
        </div>
        <div className="row">
          <button onClick={() => setView({ name: "list" })}>Servers</button>
          {user.isAdmin && <button onClick={() => setView({ name: "admin" })}>Admin</button>}
          <button onClick={() => setView({ name: "account" })}>Account</button>
          <span className="muted">
            {user.username}
            {user.isAdmin ? " (admin)" : ""}
          </span>
          <button onClick={onLogout}>Logout</button>
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
        {view.name === "admin" && user.isAdmin && <Admin />}
        {view.name === "account" && <Account user={user} />}
      </div>
    </>
  );
}
