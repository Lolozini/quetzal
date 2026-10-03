import { useEffect, useRef, useState } from "react";
import { api, ApiError } from "../api";
import { useT } from "../i18n";
import { Lockup } from "./Brand";

// ConfirmEmail is shown when the app loads with a #confirm-email=<token> link
// from a confirmation mail. It needs no session: the link may be opened on a
// phone that never signed in.
export function ConfirmEmail({ token, onDone }: { token: string; onDone: () => void }) {
  const { t } = useT();
  const [address, setAddress] = useState("");
  const [error, setError] = useState("");
  // The token works once; a development double render must not spend it twice.
  const sent = useRef(false);

  useEffect(() => {
    if (sent.current) return;
    sent.current = true;
    api
      .confirmEmail(token)
      .then((r) => setAddress(r.email))
      .catch((err) => setError(err instanceof ApiError ? err.message : String(err)));
  }, [token]);

  return (
    <div className="center auth">
      <Lockup stacked />
      <div className="card" style={{ width: 360 }}>
        {address ? (
          <p>{t("{address} is now your account's email address.", { address })}</p>
        ) : error ? (
          <div className="error">{error}</div>
        ) : (
          <p className="muted">{t("Confirming your email address…")}</p>
        )}
        <button
          type="button"
          className="primary"
          style={{ marginTop: 16, width: "100%" }}
          disabled={!address && !error}
          onClick={onDone}
        >
          {t("Continue")}
        </button>
      </div>
    </div>
  );
}
