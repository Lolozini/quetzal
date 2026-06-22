// Typed client for the Quetzal API. Cookies carry the session, so every
// request uses credentials: "include".

export interface User {
  id: number;
  username: string;
  isAdmin: boolean;
  maxServers?: number;
  maxMemoryMB?: number;
  maxCpuMilli?: number;
  twoFactorEnabled?: boolean;
  createdAt?: string;
}

// LoginResult is either the authenticated user or a 2FA challenge: when the
// account has two-factor enabled, the password step returns twoFactorRequired
// and the client must resubmit with a code.
export type LoginResult = User | { twoFactorRequired: true };

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
  "files",
  "settings",
  "delete",
] as const;

export interface FileEntry {
  name: string;
  size: number;
  dir: boolean;
}

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

export interface Hibernation {
  enabled: boolean;
  idleMinutes: number;
  wakeOnConnect?: boolean;
  proxy?: boolean;
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
  hibernation?: Hibernation;
  hibernated?: boolean;
  clusterId?: number;
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

export interface Cluster {
  id: number;
  slug: string;
  name: string;
  inCluster: boolean;
  reachable: boolean;
  version?: string;
  nodeCount?: number;
  lastCheckedAt?: string;
  statusMessage?: string;
}

export interface ClusterNode {
  name: string;
  ready: boolean;
  version: string;
  os: string;
  cpu: string;
  memory: string;
  internalIP?: string;
}

export type ChannelType = "discord" | "webhook" | "email";

export interface NotificationChannel {
  id: number;
  createdAt: string;
  updatedAt: string;
  name: string;
  type: ChannelType;
  enabled: boolean;
  serverId: number;
  events: string[];
  // config holds the non-secret settings (e.g. email host/port/from/to/tls).
  config: Record<string, string>;
  // secrets reports which secret keys are configured, without their values.
  secrets: Record<string, boolean>;
}

export interface ChannelInput {
  name: string;
  type: ChannelType;
  enabled: boolean;
  serverId: number;
  events: string[];
  config: Record<string, string>;
}

export interface EventEntry {
  id: number;
  createdAt: string;
  type: string;
  serverId?: number;
  userId?: number;
  username?: string;
  message: string;
  data?: Record<string, string>;
}

// Event types offered as filter checkboxes; an empty selection means "all".
export const EVENT_TYPES = [
  "server.running",
  "server.crashed",
  "server.hibernated",
  "server.power",
  "server.create",
  "server.delete",
  "backup.create",
  "backup.restore",
  "schedule.create",
] as const;

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
  login: (username: string, password: string, code?: string) =>
    req<LoginResult>("POST", "/api/login", { username, password, code }),
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
  setHibernation: (id: number, hibernation: Hibernation) =>
    req<Server>("PATCH", `/api/servers/${id}`, { hibernation }),
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

  // Two-factor authentication (opt-in TOTP).
  setup2FA: () => req<{ secret: string; uri: string }>("POST", "/api/me/2fa/setup"),
  enable2FA: (code: string) =>
    req<{ recoveryCodes: string[] }>("POST", "/api/me/2fa/enable", { code }),
  disable2FA: (code: string) => req<void>("POST", "/api/me/2fa/disable", { code }),
  adminDisable2FA: (uid: number) => req<void>("POST", `/api/users/${uid}/2fa/disable`),

  apiKeys: () => req<APIKey[]>("GET", "/api/apikeys"),
  createAPIKey: (name: string) =>
    req<{ key: APIKey; token: string }>("POST", "/api/apikeys", { name }),
  deleteAPIKey: (kid: number) => req<void>("DELETE", `/api/apikeys/${kid}`),

  // Multi-cluster.
  clusters: () => req<Cluster[]>("GET", "/api/clusters"),
  createCluster: (name: string, kubeconfig: string) =>
    req<Cluster>("POST", "/api/clusters", { name, kubeconfig }),
  updateCluster: (id: number, body: { name?: string; kubeconfig?: string }) =>
    req<Cluster>("PATCH", `/api/clusters/${id}`, body),
  deleteCluster: (id: number) => req<void>("DELETE", `/api/clusters/${id}`),
  testCluster: (id: number) => req<Cluster>("POST", `/api/clusters/${id}/test`),
  clusterNodes: (id: number) => req<ClusterNode[]>("GET", `/api/clusters/${id}/nodes`),

  // File manager. Content read/write use raw bodies (not JSON), so they bypass
  // the req() helper; the browser sends a same-origin Origin so CSRF passes.
  listFiles: (id: number, path: string) =>
    req<FileEntry[]>("GET", `/api/servers/${id}/files?path=${encodeURIComponent(path)}`),
  readFile: async (id: number, path: string): Promise<string> => {
    const res = await fetch(`/api/servers/${id}/files/content?path=${encodeURIComponent(path)}`, {
      credentials: "include",
    });
    if (!res.ok) {
      let msg = res.statusText;
      try { msg = (await res.json()).error || msg; } catch { /* ignore */ }
      throw new ApiError(res.status, msg);
    }
    return res.text();
  },
  writeFile: async (id: number, path: string, body: BodyInit): Promise<void> => {
    const res = await fetch(`/api/servers/${id}/files/content?path=${encodeURIComponent(path)}`, {
      method: "PUT",
      credentials: "include",
      headers: { "Content-Type": "application/octet-stream" },
      body,
    });
    if (!res.ok) {
      let msg = res.statusText;
      try { msg = (await res.json()).error || msg; } catch { /* ignore */ }
      throw new ApiError(res.status, msg);
    }
  },
  mkdir: (id: number, path: string) =>
    req<void>("POST", `/api/servers/${id}/files/mkdir?path=${encodeURIComponent(path)}`),
  renameFile: (id: number, path: string, to: string) =>
    req<void>("POST", `/api/servers/${id}/files/rename?path=${encodeURIComponent(path)}&to=${encodeURIComponent(to)}`),
  deleteFile: (id: number, path: string) =>
    req<void>("DELETE", `/api/servers/${id}/files?path=${encodeURIComponent(path)}`),
  fileDownloadUrl: (id: number, path: string) =>
    `/api/servers/${id}/files/content?path=${encodeURIComponent(path)}&download=1`,

  // Notifications.
  channels: () => req<NotificationChannel[]>("GET", "/api/notifications/channels"),
  serverChannels: (id: number) =>
    req<NotificationChannel[]>("GET", `/api/servers/${id}/notifications`),
  createChannel: (body: ChannelInput) =>
    req<NotificationChannel>("POST", "/api/notifications/channels", body),
  updateChannel: (nid: number, body: Partial<ChannelInput>) =>
    req<NotificationChannel>("PATCH", `/api/notifications/channels/${nid}`, body),
  deleteChannel: (nid: number) =>
    req<void>("DELETE", `/api/notifications/channels/${nid}`),
  testChannel: (nid: number) =>
    req<void>("POST", `/api/notifications/channels/${nid}/test`),
  events: () => req<EventEntry[]>("GET", "/api/events"),
  serverEvents: (id: number) => req<EventEntry[]>("GET", `/api/servers/${id}/events`),
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
  hibernation?: Hibernation;
  cluster?: string;
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
