import { useState } from "react";
import { User } from "../api";
import { ServerList } from "./ServerList";
import { CreateServer } from "./CreateServer";
import { ServerDetail } from "./ServerDetail";

type View =
  | { name: "list" }
  | { name: "create" }
  | { name: "detail"; id: number };

export function Dashboard({ user, onLogout }: { user: User; onLogout: () => void }) {
  const [view, setView] = useState<View>({ name: "list" });

  return (
    <>
      <div className="topbar">
        <div className="brand">
          Quetz<span>al</span>
        </div>
        <div className="row">
          <span className="muted">{user.username}</span>
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
          <ServerDetail id={view.id} onBack={() => setView({ name: "list" })} />
        )}
      </div>
    </>
  );
}
