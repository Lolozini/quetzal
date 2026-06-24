// Typed client for the Quetzal API. Cookies carry the session, so every
// request uses credentials: "include".

export interface User {
  id: number;
  username: string;
  email?: string;
  isAdmin: boolean;
  adminRoleId?: number | null;
  adminPerms?: string[];
  maxServers?: number;
  maxMemoryMB?: number;
  maxCpuMilli?: number;
  twoFactorEnabled?: boolean;
  createdAt?: string;
}

export interface AdminRole {
  id: number;
  name: string;
  description: string;
  permissions: string[];
  createdAt?: string;
  updatedAt?: string;
}

export interface AdminPermInfo {
  key: string;
  description: string;
}

// hasAdminPerm mirrors the server: superadmins hold every admin permission;
// scoped admins hold the resolved set in adminPerms.
export function hasAdminPerm(user: User, perm: string): boolean {
  return user.isAdmin || !!user.adminPerms?.includes(perm);
}

// isAnyAdmin reports whether the user has any administrative access (so the
// Admin section should be shown).
export function isAnyAdmin(user: User): boolean {
  return user.isAdmin || (user.adminPerms?.length ?? 0) > 0;
}

export interface EmailSettings {
  configured: boolean;
  host: string;
  port: string;
  username: string;
  from: string;
  tls: string;
  hasPassword: boolean;
  publicUrl: string;
}

export interface EmailSettingsInput {
  host: string;
  port: string;
  username: string;
  password: string;
  from: string;
  tls: string;
  publicUrl: string;
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
  "databases",
  "delete",
] as const;

export interface DatabaseHost {
  id: number;
  name: string;
  kind: "external" | "managed";
  host: string;
  port: number;
  connectHost: string;
  connectPort: number;
  adminUser: string;
  maxDatabases: number;
  namespace?: string;
  image?: string;
  storageSize?: string;
  reachable: boolean;
  statusMessage?: string;
  lastCheckedAt?: string;
  databases?: number;
}

export interface ServerDatabase {
  id: number;
  serverId: number;
  hostId: number;
  databaseName: string;
  username: string;
  remote: string;
  host?: string;
  port?: number;
  hostName?: string;
  password?: string; // only present on create/get/rotate
  createdAt: string;
}

export interface FileEntry {
  name: string;
  size: number;
  dir: boolean;
}

export interface SSHKey {
  id: number;
  createdAt: string;
  userId: number;
  name: string;
  publicKey: string;
  fingerprint: string;
}

export interface SFTPInfo {
  enabled: boolean;
  username: string;
  port: number;
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
  author?: string;
  category?: string;
  description?: string;
  version?: number;
  images: TemplateImage[];
  variables: TemplateVariable[];
  ports?: { name: string; port: number; protocol: string }[];
  install?: { image?: string; entrypoint?: string; script?: string };
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
  templateId?: number;
  image: string;
  env?: Record<string, string>;
  resources: { memory?: string; cpu?: string };
  storage: { type: string; size?: string; hostPath?: string };
  ports?: Port[];
  expose: Expose;
  hibernation?: Hibernation;
  hibernated?: boolean;
  sftp?: { enabled: boolean };
  clusterId?: number;
  status: ServerStatus;
}

export interface ServerStats {
  cpuMillicores: number;
  memoryBytes: number;
  cpuLimit?: string;
  memoryLimit?: string;
  // Cumulative network counters (client derives a rate) + disk usage. Present
  // only when the pod exposes them (a shell + df in the image).
  rxBytes?: number;
  txBytes?: number;
  diskTotalBytes?: number;
  diskUsedBytes?: number;
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

// rawTemplate sends a raw JSON string body (the egg / native template), not a
// JSON.stringify of an object, since the server reads the body verbatim.
async function rawTemplate(method: string, path: string, body: string): Promise<Template> {
  const res = await fetch(path, {
    method,
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body,
  });
  if (!res.ok) {
    let msg = res.statusText;
    try {
      msg = (await res.json()).error || msg;
    } catch {
      /* ignore */
    }
    if (res.status === 401) window.dispatchEvent(new Event("quetzal:unauthorized"));
    throw new ApiError(res.status, msg);
  }
  return (await res.json()) as Template;
}

export const api = {
  setupStatus: () => req<{ needed: boolean }>("GET", "/api/setup/status"),
  setup: (username: string, password: string, email?: string) =>
    req<User>("POST", "/api/setup", { username, password, email }),
  login: (username: string, password: string, code?: string) =>
    req<LoginResult>("POST", "/api/login", { username, password, code }),
  logout: () => req<void>("POST", "/api/logout"),
  me: () => req<User>("GET", "/api/me"),

  // Self-service password reset.
  forgotPassword: (identifier: string) =>
    req<{ ok: boolean }>("POST", "/api/forgot-password", { identifier }),
  resetPassword: (token: string, password: string) =>
    req<void>("POST", "/api/reset-password", { token, password }),
  setMyEmail: (email: string) => req<User>("PUT", "/api/me/email", { email }),
  // System email settings (admin).
  emailSettings: () => req<EmailSettings>("GET", "/api/email-settings"),
  setEmailSettings: (body: EmailSettingsInput) =>
    req<void>("PUT", "/api/email-settings", body),
  testEmail: (to?: string) => req<void>("POST", "/api/email-settings/test", { to }),

  templates: () => req<Template[]>("GET", "/api/templates"),
  template: (slug: string) => req<Template>("GET", `/api/templates/${slug}`),
  // Egg/template management (admin). Import/update send raw JSON bodies.
  importEgg: async (eggJson: string): Promise<Template> => rawTemplate("POST", "/api/templates/import", eggJson),
  updateTemplate: async (slug: string, templateJson: string): Promise<Template> =>
    rawTemplate("PUT", `/api/templates/${slug}`, templateJson),
  deleteTemplate: (slug: string) => req<void>("DELETE", `/api/templates/${slug}`),
  templateExportUrl: (slug: string) => `/api/templates/${slug}/export`,
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
  setServerEnv: (id: number, env: Record<string, string>) =>
    req<Server>("PATCH", `/api/servers/${id}`, { env }),
  setServerResources: (id: number, resources: { memory: string; cpu: string }) =>
    req<Server>("PATCH", `/api/servers/${id}`, { resources }),
  reinstallServer: (id: number, wipeData: boolean) =>
    req<{ status: string; wipeData: boolean }>("POST", `/api/servers/${id}/reinstall`, { wipeData }),
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
  setUserAdminRole: (uid: number, roleId: number | null) =>
    req<User>("PUT", `/api/users/${uid}/admin-role`, { roleId }),
  changePassword: (oldPassword: string, newPassword: string) =>
    req<void>("POST", "/api/me/password", { oldPassword, newPassword }),

  // Admin roles (scoped admin permission bundles; superadmin only).
  adminPermissions: () => req<AdminPermInfo[]>("GET", "/api/admin-permissions"),
  adminRoles: () => req<AdminRole[]>("GET", "/api/admin-roles"),
  createAdminRole: (body: { name: string; description: string; permissions: string[] }) =>
    req<AdminRole>("POST", "/api/admin-roles", body),
  updateAdminRole: (rid: number, body: { name: string; description: string; permissions: string[] }) =>
    req<AdminRole>("PUT", `/api/admin-roles/${rid}`, body),
  deleteAdminRole: (rid: number) => req<void>("DELETE", `/api/admin-roles/${rid}`),

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
  // Upload an archive (world/modpack/Pterodactyl backup) and extract it into a
  // directory. format is "zip" or "tar" (covers .tar.gz/.tgz/.tar.bz2/.tar.xz).
  extractArchive: async (id: number, path: string, format: "zip" | "tar", file: File): Promise<void> => {
    const res = await fetch(
      `/api/servers/${id}/files/extract?path=${encodeURIComponent(path)}&format=${format}`,
      { method: "POST", credentials: "include", headers: { "Content-Type": "application/octet-stream" }, body: file },
    );
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
  fileArchiveUrl: (id: number, path: string) =>
    `/api/servers/${id}/files/archive?path=${encodeURIComponent(path)}`,

  // SSH keys (for SFTP auth) + per-server SFTP.
  sshKeys: () => req<SSHKey[]>("GET", "/api/me/sshkeys"),
  addSSHKey: (name: string, publicKey: string) =>
    req<SSHKey>("POST", "/api/me/sshkeys", { name, publicKey }),
  deleteSSHKey: (kid: number) => req<void>("DELETE", `/api/me/sshkeys/${kid}`),
  sftpInfo: (id: number) => req<SFTPInfo>("GET", `/api/servers/${id}/sftp`),
  setSFTP: (id: number, enabled: boolean) =>
    req<Server>("PATCH", `/api/servers/${id}`, { sftp: { enabled } }),

  // Database hosts (admin) + per-server databases.
  databaseHosts: () => req<DatabaseHost[]>("GET", "/api/database-hosts"),
  createDatabaseHost: (body: Record<string, unknown>) =>
    req<DatabaseHost>("POST", "/api/database-hosts", body),
  updateDatabaseHost: (hid: number, body: Record<string, unknown>) =>
    req<DatabaseHost>("PATCH", `/api/database-hosts/${hid}`, body),
  deleteDatabaseHost: (hid: number) => req<void>("DELETE", `/api/database-hosts/${hid}`),
  testDatabaseHost: (hid: number) => req<DatabaseHost>("POST", `/api/database-hosts/${hid}/test`),
  serverDatabaseHosts: (id: number) =>
    req<{ id: number; name: string; kind: string; full: boolean }[]>("GET", `/api/servers/${id}/database-hosts`),
  serverDatabases: (id: number) => req<ServerDatabase[]>("GET", `/api/servers/${id}/databases`),
  createServerDatabase: (id: number, hostId: number) =>
    req<ServerDatabase>("POST", `/api/servers/${id}/databases`, { hostId }),
  getServerDatabase: (id: number, dbid: number) =>
    req<ServerDatabase>("GET", `/api/servers/${id}/databases/${dbid}`),
  rotateServerDatabase: (id: number, dbid: number) =>
    req<ServerDatabase>("POST", `/api/servers/${id}/databases/${dbid}/rotate`),
  deleteServerDatabase: (id: number, dbid: number) =>
    req<void>("DELETE", `/api/servers/${id}/databases/${dbid}`),

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
