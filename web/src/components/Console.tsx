import { FormEvent, useEffect, useRef, useState } from "react";
import { ConsoleMessage, consoleSocket } from "../api";
import { useT } from "../i18n";

interface Line {
  cls: string;
  text: string;
}

export function Console({ id }: { id: number }) {
  const { t } = useT();
  const [lines, setLines] = useState<Line[]>([]);
  const [input, setInput] = useState("");
  const [connected, setConnected] = useState(false);
  const wsRef = useRef<WebSocket | null>(null);
  const boxRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    const append = (cls: string, text: string) =>
      setLines((prev) => [...prev, { cls, text }].slice(-1000));

    const ws = consoleSocket(id);
    wsRef.current = ws;
    ws.onopen = () => setConnected(true);
    ws.onclose = () => {
      setConnected(false);
      append("sys", t("— disconnected —") + "\n");
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
    return () => ws.close();
  }, [id]);

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
