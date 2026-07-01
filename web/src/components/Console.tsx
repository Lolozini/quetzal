import { FormEvent, useEffect, useRef, useState } from "react";
import { ConsoleMessage, consoleSocket } from "../api";
import { useT } from "../i18n";

interface Line {
  cls: string;
  text: string;
}

// A server with no pod (Stopped/Suspended/Hibernated) has nothing to stream, so
// we don't hold a socket open against it; any other phase is treated as live.
const offlinePhases = ["", "Stopped", "Suspended", "Hibernated"];

export function Console({ id, phase }: { id: number; phase: string }) {
  const { t } = useT();
  const [lines, setLines] = useState<Line[]>([]);
  const [input, setInput] = useState("");
  const [connected, setConnected] = useState(false);
  const wsRef = useRef<WebSocket | null>(null);
  const boxRef = useRef<HTMLDivElement | null>(null);
  const live = !offlinePhases.includes(phase);

  useEffect(() => {
    if (!live) return;
    const append = (cls: string, text: string) =>
      setLines((prev) => [...prev, { cls, text }].slice(-1000));

    let stopped = false;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let wasOpen = false;

    // Self-healing connection: while the server is live we keep a socket open,
    // reconnecting after a short delay if it drops (e.g. the pod is still coming
    // up after a start). "— disconnected —" is only shown once an established
    // console actually drops, so silent reconnect attempts don't spam the log.
    const connect = () => {
      const ws = consoleSocket(id);
      wsRef.current = ws;
      ws.onopen = () => {
        wasOpen = true;
        setConnected(true);
      };
      ws.onclose = () => {
        setConnected(false);
        if (stopped) return;
        if (wasOpen) append("sys", t("— disconnected —") + "\n");
        wasOpen = false;
        retry = setTimeout(connect, 3000);
      };
      ws.onmessage = (ev) => {
        try {
          const m: ConsoleMessage = JSON.parse(ev.data);
          if (m.type === "stdout") append("", m.data);
          else if (m.type === "status") append("sys", m.data + "\n");
          else if (m.type === "error") append("err", m.data + "\n");
        } catch {
          /* ignore non-JSON frames */
        }
      };
    };
    connect();
    return () => {
      stopped = true;
      clearTimeout(retry);
      wsRef.current?.close();
    };
  }, [id, live]);

  useEffect(() => {
    const el = boxRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [lines]);

  function send(e: FormEvent) {
    e.preventDefault();
    const ws = wsRef.current;
    if (!ws || ws.readyState !== WebSocket.OPEN || !input) return;
    ws.send(JSON.stringify({ type: "stdin", data: input }));
    setLines((prev) => [...prev, { cls: "sys", text: "> " + input + "\n" }]);
    setInput("");
  }

  return (
    <div>
      <div className="row">
        <h3>{t("Console")}</h3>
        <span className={`badge ${connected ? "Running" : "Stopped"}`}>
          {connected ? t("connected") : t("disconnected")}
        </span>
      </div>
      <div className="console" ref={boxRef}>
        {lines.map((l, i) => (
          <span key={i} className={l.cls}>
            {l.text}
          </span>
        ))}
      </div>
      <form className="console-input" onSubmit={send}>
        <input
          placeholder={t("type a command and press Enter…")}
          value={input}
          onChange={(e) => setInput(e.target.value)}
          disabled={!connected}
        />
        <button className="primary" disabled={!connected}>
          {t("Send")}
        </button>
      </form>
    </div>
  );
}
