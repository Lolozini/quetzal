// Typed client for the Quetzal API. Cookies carry the session, so every
// request uses credentials: "include".

export interface User {
  id: number;
  username: string;
  isAdmin: boolean;
  maxServers?: number;
  maxMemoryMB?: number;
  maxCpuMilli?: number;
  createdAt?: string;
}

export interface ServerAccess {
  id: number;
  serverId: number;
  userId: number;
  username?: string;
  permissions: string[];
}

export const ALL_PERMISSIONS = [
  "view",
  "power",
  "console",
  "schedules",
  "backups",
  "settings",
  "delete",
] as const;

export interface AuditEntry {
  id: number;
  createdAt: string;
  userId: number;
  username: string;
  serverId?: number;
  action: string;
  detail?: string;
}

export interface APIKey {
  id: number;
  userId: number;
  name: string;
  prefix: string;
  createdAt: string;
  lastUsedAt?: string;
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
  ownerId?: number;
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

export interface BackupConfig {
  endpoint: string;
  bucket: string;
  prefix: string;
  region: string;
  useSSL: boolean;
  keepLast: number;
  runnerImage: string;
  configured: boolean;
  hasCredentials: boolean;
  hasPassword: boolean;
}

export interface BackupConfigInput {
  endpoint: string;
  bucket: string;
  prefix?: string;
  region?: string;
  useSSL: boolean;
  keepLast: number;
  runnerImage?: string;
  accessKey?: string;
  secretKey?: string;
  repoPassword?: string;
}

export type BackupDirection = "backup" | "restore";
export type BackupPhase = "Pending" | "Running" | "Succeeded" | "Failed";

export interface Backup {
  id: number;
  serverId: number;
  direction: BackupDirection;
  phase: BackupPhase;
  sourceId?: number;
  sizeBytes?: number;
  message?: string;
  createdAt: string;
  completedAt?: string;
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

  backupConfig: () => req<BackupConfig>("GET", "/api/backup-config"),
  setBackupConfig: (body: BackupConfigInput) => req<void>("PUT", "/api/backup-config", body),
  backups: (id: number) => req<Backup[]>("GET", `/api/servers/${id}/backups`),
  createBackup: (id: number) => req<Backup>("POST", `/api/servers/${id}/backups`),
  restoreBackup: (id: number, bid: number) =>
    req<Backup>("POST", `/api/servers/${id}/backups/${bid}/restore`),
  deleteBackup: (id: number, bid: number) =>
    req<void>("DELETE", `/api/servers/${id}/backups/${bid}`),

  // Multi-tenant.
  suspend: (id: number) => req<void>("POST", `/api/servers/${id}/suspend`),
  unsuspend: (id: number) => req<void>("POST", `/api/servers/${id}/unsuspend`),
  access: (id: number) => req<ServerAccess[]>("GET", `/api/servers/${id}/access`),
  grantAccess: (id: number, username: string, permissions: string[]) =>
    req<void>("POST", `/api/servers/${id}/access`, { username, permissions }),
  revokeAccess: (id: number, uid: number) =>
    req<void>("DELETE", `/api/servers/${id}/access/${uid}`),
  serverAudit: (id: number) => req<AuditEntry[]>("GET", `/api/servers/${id}/audit`),
  globalAudit: () => req<AuditEntry[]>("GET", "/api/audit"),

  users: () => req<User[]>("GET", "/api/users"),
  createUser: (body: Record<string, unknown>) => req<User>("POST", "/api/users", body),
  updateUser: (uid: number, body: Record<string, unknown>) =>
    req<User>("PATCH", `/api/users/${uid}`, body),
  deleteUser: (uid: number) => req<void>("DELETE", `/api/users/${uid}`),
  changePassword: (oldPassword: string, newPassword: string) =>
    req<void>("POST", "/api/me/password", { oldPassword, newPassword }),

  apiKeys: () => req<APIKey[]>("GET", "/api/apikeys"),
  createAPIKey: (name: string) =>
    req<{ key: APIKey; token: string }>("POST", "/api/apikeys", { name }),
  deleteAPIKey: (kid: number) => req<void>("DELETE", `/api/apikeys/${kid}`),
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
