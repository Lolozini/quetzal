// Typed client for the Quetzal API. Cookies carry the session, so every
// request uses credentials: "include".

export interface User {
  id: number;
  username: string;
  isAdmin: boolean;
}

export interface TemplateVariable {
  name: string;
  description?: string;
  envVariable: string;
  type: string;
  default?: string;
  required?: boolean;
  options?: string[];
  editable: boolean;
}

export interface TemplateImage {
  displayName: string;
  ref: string;
  default?: boolean;
}

export interface Template {
  id: number;
  slug: string;
  name: string;
  category?: string;
  description?: string;
  images: TemplateImage[];
  variables: TemplateVariable[];
  ports?: { name: string; port: number; protocol: string }[];
}

export interface ServerStatus {
  phase: string;
  endpoints?: string[];
  message?: string;
}

export interface Server {
  id: number;
  slug: string;
  displayName: string;
  namespace: string;
  desiredState: string;
  image: string;
  resources: { memory?: string; cpu?: string };
  storage: { type: string; size?: string; hostPath?: string };
  status: ServerStatus;
}

export type PowerAction = "start" | "stop" | "restart" | "kill";

async function req<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: "include",
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const data = await res.json();
      if (data?.error) msg = data.error;
    } catch {
      /* ignore */
    }
    throw new ApiError(res.status, msg);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

export const api = {
  setupStatus: () => req<{ needed: boolean }>("GET", "/api/setup/status"),
  setup: (username: string, password: string) =>
    req<User>("POST", "/api/setup", { username, password }),
  login: (username: string, password: string) =>
    req<User>("POST", "/api/login", { username, password }),
  logout: () => req<void>("POST", "/api/logout"),
  me: () => req<User>("GET", "/api/me"),

  templates: () => req<Template[]>("GET", "/api/templates"),
  servers: () => req<Server[]>("GET", "/api/servers"),
  server: (id: number) => req<Server>("GET", `/api/servers/${id}`),
  createServer: (body: CreateServerRequest) =>
    req<Server>("POST", "/api/servers", body),
  deleteServer: (id: number) => req<void>("DELETE", `/api/servers/${id}`),
  power: (id: number, action: PowerAction) =>
    req<{ action: string; result: string }>("POST", `/api/servers/${id}/power`, {
      action,
    }),
};

export interface CreateServerRequest {
  name: string;
  template: string;
  image?: string;
  memory?: string;
  cpu?: string;
  storage?: { type: string; size?: string; hostPath?: string };
  env?: Record<string, string>;
  start?: boolean;
}

// consoleSocket opens the live console WebSocket for a server.
export function consoleSocket(id: number): WebSocket {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  return new WebSocket(`${proto}://${location.host}/api/servers/${id}/console`);
}

export interface ConsoleMessage {
  type: "stdout" | "stdin" | "status" | "error";
  data: string;
}
