import { useEffect, useState } from "react";
import { api, hasAdminPerm, OrphanNamespaces, User } from "../api";
import { useT } from "../i18n";

// How often an administrator's open page asks again. The report changes on the
// controller's pass, and the hour before a namespace goes is what it is for.
const REFRESH_MS = 60_000;

/**
 * OrphanBanner tells an administrator that some namespaces have no server in the
 * database. That is what a database restored from a backup looks like, and the
 * controller deletes such a namespace, volume included, after an hour (or, when
 * there are too many at once, leaves them alone). Without this the only trace
 * was a line in the controller's log. Renders nothing when all is well, and for
 * anyone who cannot act on it.
 */
export function OrphanBanner({ user }: { user: User }) {
  const { t } = useT();
  const allowed = hasAdminPerm(user, "servers");
  const [found, setFound] = useState<OrphanNamespaces[]>([]);

  useEffect(() => {
    if (!allowed) return;
    let live = true;
    const load = () => {
      api.orphanNamespaces().then((r) => live && setFound(r)).catch(() => {
        // The banner is a courtesy: a failed read leaves the last answer.
      });
    };
    load();
    const id = window.setInterval(load, REFRESH_MS);
    return () => {
      live = false;
      window.clearInterval(id);
    };
  }, [allowed]);

  if (!allowed || found.length === 0) return null;
  return (
    <div className="container">
      {found.map((o) => {
        const hours = Math.max(1, Math.round(o.graceSeconds / 3600));
        const list = o.names.slice(0, 8).join(", ") + (o.names.length > 8 ? ` +${o.names.length - 8}` : "");
        return (
          <div key={o.clusterId} className="notice warn" role="alert">
            <strong>
              {t("{n} of {total} namespaces on {cluster} have no server in the database", {
                n: o.names.length,
                total: o.managed,
                cluster: o.cluster,
              })}
            </strong>
            <div>{list}</div>
            <div>
              {o.refused
                ? t("That is too many to delete on its own, so none is deleted. If the database was restored from a backup, they are not orphans: delete the namespaces by hand once you are sure.")
                : t("Each is deleted, volume included, after {hours} h unless its server comes back. If the database was just restored from a backup, stop the controller and check before then.", { hours })}
            </div>
          </div>
        );
      })}
    </div>
  );
}
