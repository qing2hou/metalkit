import { apiGet, apiSend } from './client'
import type {
  AuditEvent,
  AuthUser,
  Binding,
  BmcActionResult,
  BmcCredential,
  BmcTestResult,
  DhcpSettings,
  DhcpSettingsResponse,
  InterfaceInfo,
  Image,
  Job,
  JobLog,
  MachineSummary,
  Profile,
  ProfileComponents,
  Report,
  ReportMeta,
  Subnet,
  UploadSession,
} from './types'

// ---------- auth ----------

export const authApi = {
  login: (username: string, password: string) =>
    apiSend<AuthUser>('POST', '/auth/login', { username, password }),
  logout: () => apiSend<null>('POST', '/auth/logout'),
  me: () => apiGet<AuthUser>('/auth/me'),
}

// ---------- machines ----------

export const machinesApi = {
  list: () => apiGet<MachineSummary[]>('/machines'),
  get: (uuid: string) => apiGet<Report>(`/machines/${uuid}`),
  reports: (uuid: string) => apiGet<ReportMeta[]>(`/machines/${uuid}/reports`),
  report: (uuid: string, id: number) => apiGet<Report>(`/machines/${uuid}/reports/${id}`),
  remove: (uuid: string) => apiSend<null>('DELETE', `/machines/${uuid}`),
}

// ---------- images ----------

export const imagesApi = {
  list: () => apiGet<Image[]>('/images'),
  get: (id: string) => apiGet<Image>(`/images/${id}`),
  remove: (id: string) => apiSend<Image>('DELETE', `/images/${id}`),
  initUpload: (input: {
    name: string
    version: string
    family: string
    notes?: string
    expected_sha256?: string
    total_size: number
    chunk_size: number
  }) => apiSend<UploadSession>('POST', '/images/uploads', input),
  getUpload: (uid: string) => apiGet<UploadSession>(`/images/uploads/${uid}`),
  finalize: (uid: string) => apiSend<Image>('POST', `/images/uploads/${uid}/finalize`),
  abortUpload: (uid: string) => apiSend<null>('DELETE', `/images/uploads/${uid}`),
}

// ---------- profiles ----------

export const profilesApi = {
  list: () => apiGet<Profile[]>('/profiles'),
  get: (id: string) => apiGet<Profile>(`/profiles/${id}`),
  create: (input: Record<string, unknown>) => apiSend<Profile>('POST', '/profiles', input),
  update: (id: string, input: Record<string, unknown>) =>
    apiSend<Profile>('PUT', `/profiles/${id}`, input),
  remove: (id: string) => apiSend<null>('DELETE', `/profiles/${id}`),
  components: (osFamily: string) =>
    apiGet<ProfileComponents>(`/profiles/components?os_family=${encodeURIComponent(osFamily)}`),
}

// ---------- subnets ----------

export const subnetsApi = {
  list: () => apiGet<Subnet[]>('/subnets'),
  get: (id: string) => apiGet<Subnet>(`/subnets/${id}`),
  create: (input: Record<string, unknown>) => apiSend<Subnet>('POST', '/subnets', input),
  update: (id: string, input: Record<string, unknown>) =>
    apiSend<Subnet>('PUT', `/subnets/${id}`, input),
  remove: (id: string) => apiSend<null>('DELETE', `/subnets/${id}`),
}

// ---------- bindings ----------

export const bindingsApi = {
  list: () => apiGet<Binding[]>('/bindings'),
  get: (uuid: string) => apiGet<Binding>(`/bindings/${uuid}`),
  upsert: (uuid: string, input: Record<string, unknown>) =>
    apiSend<Binding>('PUT', `/bindings/${uuid}`, input),
  remove: (uuid: string) => apiSend<null>('DELETE', `/bindings/${uuid}`),
  password: (uuid: string) => apiGet<{ password: string }>(`/bindings/${uuid}/password`),
}

// ---------- bmc ----------

export const bmcApi = {
  list: () => apiGet<BmcCredential[]>('/bmc'),
  get: (uuid: string) => apiGet<BmcCredential>(`/bmc/${uuid}`),
  create: (input: Record<string, unknown>) => apiSend<BmcCredential>('POST', '/bmc', input),
  update: (uuid: string, input: Record<string, unknown>) =>
    apiSend<BmcCredential>('PUT', `/bmc/${uuid}`, input),
  remove: (uuid: string) => apiSend<null>('DELETE', `/bmc/${uuid}`),
  test: (uuid: string) => apiSend<BmcTestResult>('POST', `/bmc/${uuid}/test`, {}),
  power: (uuid: string, action: 'on' | 'off' | 'cycle' | 'soft' | 'reset') =>
    apiSend<BmcActionResult>('POST', `/bmc/${uuid}/power/${action}`, {}),
  onboard: (uuid: string) => apiSend<BmcActionResult>('POST', `/bmc/${uuid}/onboard`, {}),
}

// ---------- jobs ----------

export const jobsApi = {
  list: (params: { machine_uuid?: string; status?: string; limit?: number } = {}) => {
    const qs = new URLSearchParams()
    if (params.machine_uuid) qs.set('machine_uuid', params.machine_uuid)
    if (params.status) qs.set('status', params.status)
    if (params.limit) qs.set('limit', String(params.limit))
    const s = qs.toString()
    return apiGet<Job[]>(`/jobs${s ? `?${s}` : ''}`)
  },
  get: (id: string) => apiGet<Job>(`/jobs/${id}`),
  logs: (id: string, sinceId = 0, limit = 200) =>
    apiGet<JobLog[]>(`/jobs/${id}/logs?since_id=${sinceId}&limit=${limit}`),
  cancel: (id: string) => apiSend<null>('POST', `/jobs/${id}/cancel`, {}),
  remove: (id: string) => apiSend<null>('DELETE', `/jobs/${id}`),
  purge: () => apiSend<{ deleted: number }>('POST', '/jobs/purge', {}),
}

// ---------- settings ----------

export const settingsApi = {
  getDhcp: () => apiGet<DhcpSettingsResponse>('/settings/dhcp'),
  putDhcp: (settings: DhcpSettings) => apiSend<DhcpSettingsResponse>('PUT', '/settings/dhcp', settings),
  interfaces: () => apiGet<InterfaceInfo[]>('/settings/interfaces'),
}

// ---------- audit ----------

export const auditApi = {
  list: (params: { actor?: string; action?: string; limit?: number; offset?: number } = {}) => {
    const qs = new URLSearchParams()
    if (params.actor) qs.set('actor', params.actor)
    if (params.action) qs.set('action', params.action)
    if (params.limit) qs.set('limit', String(params.limit))
    if (params.offset) qs.set('offset', String(params.offset))
    const q = qs.toString()
    return apiGet<AuditEvent[]>(`/audit${q ? `?${q}` : ''}`)
  },
}

// ---------- util ----------

export const utilApi = {
  cryptSha512: (password: string) =>
    apiSend<{ hash: string }>('POST', '/util/crypt-sha512', { password }),
}
