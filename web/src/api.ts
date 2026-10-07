import { retryAfterSeconds } from './shared/followOperation';

export type User = { id: string; username: string; role: string };
export type Organization = { id: string; name: string; slug: string; role: string; state: string };
export type Page<T> = { items: T[]; next_cursor: string | null };
export type Project = {
  effective_permission: 'admin' | 'editor' | 'viewer' | 'none';
  id: string;
  org_id: string;
  name: string;
  region_id: string;
  postgres_version: number;
  state: string;
  source: string;
  default_branch_id: string | null;
  created_at: string;
};
export type Branch = {
  id: string;
  project_id: string;
  name: string;
  parent_branch_id: string | null;
  parent_lsn: string | null;
  parent_timestamp: string | null;
  restore_source: 'current' | 'timestamp' | 'lsn';
  is_default: boolean;
  protected: boolean;
  state: string;
  created_at: string;
};
export type BranchService = {
  service_kind: string;
  desired_state: string;
  observed_state: string;
  driver_version?: string | null;
  public_endpoint?: string | null;
  enabled: boolean;
  reason: string;
};
export type Runtime = {
  observed_state: string;
  observed_at: string;
  phase?: string;
  cpu_milli?: number;
  memory_mib?: number;
  pod_name?: string;
  node_name?: string;
  workload_uid?: string;
  workload_created_at?: string;
  error?: string;
};
export type Endpoint = {
  id: string;
  endpoint_type: 'read_write' | 'read_only';
  project_id: string;
  branch_id: string;
  selector: string;
  state: string;
  observed_state: string;
  workload_kind: string;
  role_name: string;
  database_name: string;
  version: number;
  scale_to_zero: boolean;
  idle_timeout_seconds: number;
  min_cpu_milli: number | null;
  max_cpu_milli: number | null;
  min_memory_mib: number | null;
  max_memory_mib: number | null;
  runtime: Runtime;
};
export type Feature = { enabled: boolean; reason: string };
export type Capabilities = {
  cluster_id: string;
  observed_at: string;
  features: Record<string, Feature>;
  services: Record<string, Feature>;
  runtime?: RuntimeStatus;
};
export type RuntimeStatus = {
  process_role: 'all' | 'api' | 'worker';
  separated: boolean;
  controller_status: 'active' | 'stale' | 'unavailable';
  epoch?: number;
  last_heartbeat_at?: string;
};
export type OperationStep = {
  ordinal: number;
  name: string;
  state: string;
  attempts: number;
  detail: string | null;
  updated_at: string;
};
export type Operation = {
  id: string;
  action: string;
  state: string;
  resource_id: string;
  error_code: string | null;
  error_message: string | null;
  retryable: boolean;
  created_at: string;
  finished_at: string | null;
  steps: OperationStep[];
};
export type Metric = {
  sampled_at: string;
  observed_state: string;
  cpu_allocated_milli: number | null;
  cpu_used_milli: number | null;
  memory_allocated_mib: number | null;
  memory_used_mib: number | null;
  connections: number | null;
  active_connections: number | null;
  database_size_bytes: number | null;
  errors: Record<string, string>;
};
export type MetricHistory = {
  endpoint_id: string;
  period: string;
  sample_count: number;
  latest_age_seconds: number | null;
  fresh: boolean;
  sources: Record<string, string>;
  items: Metric[];
};
export type ConnectionInfo = {
  host: string;
  port: number;
  role: string;
  database: string;
  ssl_mode: string;
  endpoint_selector: string;
  connection_uri_template: string;
  password_included: boolean;
};
export type QueryResult = {
  columns: string[];
  rows: unknown[][];
  command: string;
  truncated: boolean;
};

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public requestId?: string,
    public retryAfterSeconds?: number,
  ) {
    super(message);
  }
}

function csrf(): string {
  const part = document.cookie.split('; ').find((value) => value.startsWith('neon_v2_csrf='));
  return part ? decodeURIComponent(part.split('=')[1]) : '';
}

export async function api<T>(path: string, options: RequestInit = {}): Promise<T> {
  const method = options.method?.toUpperCase() || 'GET';
  const headers = new Headers(options.headers);
  if (options.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
  if (method !== 'GET' && method !== 'HEAD') headers.set('X-CSRF-Token', csrf());
  const response = await fetch(path, { ...options, headers, credentials: 'same-origin' });
  const data = (await response.json().catch(() => ({}))) as Record<string, unknown>;
  if (!response.ok)
    throw new ApiError(
      response.status,
      String(data.code || 'request_failed'),
      String(data.message || `HTTP ${response.status}`),
      String(data.request_id || ''),
      retryAfterSeconds(response.headers.get('Retry-After')),
    );
  return data as T;
}

export const projectPath = (id: string) => `/api/v1/projects/${encodeURIComponent(id)}`;
export const endpointPath = (project: string, endpoint: string) =>
  `${projectPath(project)}/endpoints/${encodeURIComponent(endpoint)}`;
export const route = (project: string, page = '') =>
  `#/projects/${encodeURIComponent(project)}${page ? '/' + page : ''}`;
