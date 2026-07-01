import { useT } from "../i18n";

/**
 * RestartHint is a small inline chip that tells the user whether saving a change
 * bounces the server. `restarts` (default) means the pod rolls on the next
 * reconcile; `live` means it applies without a restart (Service/policy only).
 */
export function RestartHint({ live = false }: { live?: boolean }) {
  const { t } = useT();
  return (
    <span className={"restart-hint" + (live ? " live" : "")}>
      {live ? t("applied without a restart") : t("restarts the server")}
    </span>
  );
}
