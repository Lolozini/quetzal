import { FormEvent, KeyboardEvent, useEffect, useRef, useState } from "react";
import { ConsoleMessage, consoleSocket, OFFLINE_PHASES } from "../api";
import { useT } from "../i18n";

interface Line {
  cls: string;
  text: string;
}

// Sent commands are kept per server, newest first, in this browser only: the
// arrow keys bring them back, as in a shell or Pterodactyl's console.
const HISTORY_MAX = 50;
const historyKey = (id: number) => `quetzal.console-history.${id}`;

function loadHistory(id: number): string[] {
  try {
    const v: unknown = JSON.parse(localStorage.getItem(historyKey(id)) ?? "[]");
    return Array.isArray(v) ? v.filter((c): c is string => typeof c === "string").slice(0, HISTORY_MAX) : [];
  } catch {
    return [];
  }
}

function saveHistory(id: number, history: string[]) {
  try {
    localStorage.setItem(historyKey(id), JSON.stringify(history));
  } catch {
    /* storage unavailable: the history lasts as long as the page */
  }
}

export function Console({ id, phase }: { id: number; phase: string }) {
  const { t } = useT();
  const [lines, setLines] = useState<Line[]>([]);
  const [input, setInput] = useState("");
  const [history, setHistory] = useState<string[]>(() => loadHistory(id));
  // Position in the history while browsing it (-1: editing a new command), and
  // what was being typed before browsing started, restored on the way back down.
  const [historyPos, setHistoryPos] = useState(-1);
  const [draft, setDraft] = useState("");
  const [connected, setConnected] = useState(false);
  const wsRef = useRef<WebSocket | null>(null);
  const boxRef = useRef<HTMLDivElement | null>(null);
  // A server with no pod has nothing to stream, so we hold no socket against it.
  const live = !OFFLINE_PHASES.includes(phase);

  useEffect(() => {
    if (!live) return;
    const append = (cls: string, text: string) =>
      setLines((prev) => [...prev, { cls, text }].slice(-1000));

    let stopped = false;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let wasOpen = false;
    // Back off between attempts so a server that never comes up (a crash loop, a
    // slow image pull) doesn't hammer the apiserver from every open tab. Reset
    // once a session actually establishes, so a normal restart reconnects fast.
    let delay = 1000;
    const maxDelay = 30000;

    // Self-healing connection: while the server is live we keep a socket open,
    // reconnecting after a short delay if it drops (e.g. the pod is still coming
    // up after a start). "— disconnected —" is only shown once an established
    // console actually drops, so silent reconnect attempts don't spam the log.
    const connect = () => {
      const ws = consoleSocket(id);
      wsRef.current = ws;
      ws.onopen = () => {
        wasOpen = true;
        delay = 1000;
        setConnected(true);
      };
      ws.onclose = () => {
        setConnected(false);
        if (stopped) return;
        if (wasOpen) append("sys", t("— disconnected —") + "\n");
        wasOpen = false;
        retry = setTimeout(connect, delay);
        delay = Math.min(delay * 2, maxDelay);
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

  useEffect(() => {
    setHistory(loadHistory(id));
    setHistoryPos(-1);
  }, [id]);

  function send(e: FormEvent) {
    e.preventDefault();
    const ws = wsRef.current;
    if (!ws || ws.readyState !== WebSocket.OPEN || !input) return;
    ws.send(JSON.stringify({ type: "stdin", data: input }));
    setLines((prev) => [...prev, { cls: "sys", text: "> " + input + "\n" }]);
    // The same command sent twice in a row is kept once.
    const next = (history[0] === input ? history : [input, ...history]).slice(0, HISTORY_MAX);
    setHistory(next);
    saveHistory(id, next);
    setHistoryPos(-1);
    setDraft("");
    setInput("");
  }

  function browseHistory(e: KeyboardEvent<HTMLInputElement>) {
    if (e.nativeEvent.isComposing) return;
    if (e.key === "ArrowUp") {
      e.preventDefault();
      if (historyPos + 1 >= history.length) return;
      if (historyPos === -1) setDraft(input);
      setHistoryPos(historyPos + 1);
      setInput(history[historyPos + 1]);
    } else if (e.key === "ArrowDown") {
      if (historyPos === -1) return;
      e.preventDefault();
      setHistoryPos(historyPos - 1);
      setInput(historyPos - 1 === -1 ? draft : history[historyPos - 1]);
    }
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
          onKeyDown={browseHistory}
          disabled={!connected}
        />
        <button className="primary" disabled={!connected}>
          {t("Send")}
        </button>
      </form>
    </div>
  );
}
