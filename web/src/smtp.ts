import type { TFunc } from "./i18n";

// tlsHint warns about a TLS mode that does not suit the port. A server on 465
// expects TLS from the first byte, while one on 587 or 25 greets in plain text
// and offers STARTTLS: either mistake only shows when an email is sent, as a
// failure that does not point at the setting.
export function tlsHint(t: TFunc, port: string | undefined, mode: string | undefined): string {
  const p = (port || "").trim() || "587";
  const m = mode || "starttls";
  if (p === "465" && m !== "tls") {
    return t("Port 465 usually expects TLS from the first byte: choose implicit TLS.");
  }
  if ((p === "587" || p === "25") && m === "tls") {
    return t("Port {port} usually greets in plain text and offers STARTTLS: choose STARTTLS.", { port: p });
  }
  return "";
}
