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
  secret?: boolean;
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
  address?: string;
  message?: string;
}

export type ExposeType = "ClusterIP" | "NodePort" | "LoadBalancer";

export interface Expose {
  type?: ExposeType;
  annotations?: Record<string, string>;
  preserveClientIP?: boolean;
}

export interface Port {
  name: string;
  port: number;
  protocol: string;
  primary?: boolean;
  nodePort?: number;
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
  ports?: Port[];
  expose: Expose;
  status: ServerStatus;
}

export interface ServerStats {
  cpuMillicores: number;
  memoryBytes: number;
  cpuLimit?: string;
  memoryLimit?: string;
}

export type ScheduleAction = "start" | "stop" | "restart" | "command" | "backup";

export interface Schedule {
  id: number;
  serverId: number;
  name: string;
  cron: string;
  action: ScheduleAction;
  payload?: string;
  enabled: boolean;
  nextRun?: string;
  lastRun?: string;
  lastStatus?: string;
}

export interface ScheduleInput {
  name: string;
  cron: string;
  action: ScheduleAction;
  payload?: string;
  enabled: boolean;
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
    // Session expired/invalid: let the app return to the login screen.
    if (res.status === 401) {
      window.dispatchEvent(new Event("quetzal:unauthorized"));
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
  deleteServer: (id: number, keepData: boolean) =>
    req<void>("DELETE", `/api/servers/${id}?keepData=${keepData}`),
  power: (id: number, action: PowerAction) =>
    req<{ action: string; result: string }>("POST", `/api/servers/${id}/power`, {
      action,
    }),
  setExpose: (id: number, expose: Expose) =>
    req<Server>("PATCH", `/api/servers/${id}`, { expose }),
  stats: (id: number) => req<ServerStats>("GET", `/api/servers/${id}/stats`),

  schedules: (id: number) => req<Schedule[]>("GET", `/api/servers/${id}/schedules`),
  createSchedule: (id: number, body: ScheduleInput) =>
    req<Schedule>("POST", `/api/servers/${id}/schedules`, body),
  updateSchedule: (id: number, sid: number, body: ScheduleInput) =>
    req<Schedule>("PATCH", `/api/servers/${id}/schedules/${sid}`, body),
  deleteSchedule: (id: number, sid: number) =>
    req<void>("DELETE", `/api/servers/${id}/schedules/${sid}`),
};

export interface CreateServerRequest {
  name: string;
  template: string;
  image?: string;
  memory?: string;
  cpu?: string;
  storage?: { type: string; size?: string; hostPath?: string };
  env?: Record<string, string>;
  expose?: Expose;
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
