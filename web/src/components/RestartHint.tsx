import { useT } from "../i18n";

/**
 * RestartHint is a small inline chip warning that saving the current pending
 * change rolls the server pod on the next reconcile. Shown only while a section
 * has unsaved edits.
 */
export function RestartHint() {
  const { t } = useT();
  return <span className="restart-hint">{t("restarts the server")}</span>;
}
