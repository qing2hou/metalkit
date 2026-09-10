/**
 * 类型定义：与 Go 后端 (internal/*) 的 JSON 契约一一对应。
 * 字段名与序列化保持一致（Go struct tag snake_case）。
 */

// ---------- inventory ----------

export interface MachineSummary {
  uuid: string
  serial: string
  manufacturer: string
  product_name: string
  first_seen: string
  last_seen: string
  status: string
  latest_report: number
  bmc_ip?: string
  bmc_managed?: boolean
}

export interface ReportMeta {
  id: number
  ts: string
}

export interface Dimm {
  slot?: string
  size_bytes?: number
  speed_mt_s?: number
  manufacturer?: string
  form_factor?: string
  type?: string
}

export interface Disk {
  kname: string
  path: string
  type: string
  size_bytes: number
  model?: string
  serial?: string
  rotational?: boolean
  transport?: string
  wwn?: string
  smart?: Record<string, unknown>
  nvme?: Record<string, unknown>
}

export interface Nic {
  name: string
  mac: string
  permanent_mac?: string
  speed_mbps?: number
  duplex?: string
  link?: boolean
  mtu?: number
  driver?: string
  pci_address?: string
  /** CIDR 字符串，如 "192.168.50.20/24" */
  addresses?: string[]
  sfp?: Record<string, unknown>
}

export interface Report {
  schema_version?: number
  agent_version?: string
  collected_at: string
  collection_duration_ms?: number
  machine?: {
    smbios_uuid?: string
    manufacturer?: string
    product_name?: string
    serial?: string
    baseboard?: Record<string, unknown>
    chassis?: Record<string, unknown>
  }
  firmware?: Record<string, unknown>
  cpu?: {
    sockets?: number
    total_cores?: number
    total_threads?: number
    per_socket?: Array<Record<string, unknown>>
    flags?: string[]
  }
  memory?: {
    total_bytes?: number
    ecc?: boolean
    dimms?: Dimm[]
  }
  disks?: Disk[]
  nics?: Nic[]
  pci_devices?: Array<Record<string, unknown>>
  accelerators?: Array<Record<string, unknown>>
  bmc?: Record<string, unknown>
  sensors?: Array<Record<string, unknown>>
  system?: Record<string, unknown>
  agent?: { version?: string; errors?: string[] }
}

// ---------- images ----------

export interface Image {
  id: string
  name: string
  version: string
  family: string
  format: string
  size_bytes: number
  virtual_size?: number
  sha256?: string
  uploaded_at: string
  uploaded_by?: string
  last_used_at?: string | null
  notes?: string
  metadata_json?: string
}

export interface UploadSession {
  id: string
  name: string
  version: string
  family: string
  notes?: string
  expected_sha256?: string
  total_size: number
  chunk_size: number
  num_chunks: number
  uploaded_chunks: number
  uploaded_by?: string
  started_at: string
}

// ---------- profiles ----------

export type TargetDiskMode = 'smallest' | 'by-path' | 'by-wwn' | 'by-model'

export interface TargetDisk {
  mode: TargetDiskMode
  value?: string
}

export interface BondConfig {
  mode?: string
  slaves?: string[]
  miimon?: number
  lacp_rate?: string
  xmit_hash_policy?: string
  primary?: string
}

export interface NetworkConfig {
  method: 'dhcp' | 'static'
  prefix_len?: number
  gateway?: string
  dns?: string[]
  /** "auto" | "by-mac:<MAC>" | "by-name:<ifname>"；设置 bond 时后端强制 auto */
  nic_selector?: string
  vlan?: number
  bond?: BondConfig | null
}

export interface ProfileComponents {
  renderers: Array<{ id: string; label: string; description?: string }>
  bootloaders: Array<{ id: string; label: string; description?: string }>
}

export interface Profile {
  id: string
  name: string
  os_family: string
  hostname_template?: string
  root_password_hash?: string
  target_disk?: TargetDisk
  network?: NetworkConfig
  network_renderer?: string
  bootloader?: string
  created_at?: string
  updated_at?: string
}

// ---------- subnets ----------

export interface Subnet {
  id: string
  name: string
  description?: string
  cidr: string
  gateway?: string
  dns?: string[]
  vlan_id?: number
  created_at?: string
  updated_at?: string
}

// ---------- bindings ----------

export type DesiredState = 'live' | 'install' | 'reinstall' | 'none'

export interface Binding {
  machine_uuid: string
  image_id?: string
  profile_id?: string
  desired_state?: DesiredState
  static_address?: string
  hostname?: string
  target_disk?: TargetDisk | null
  bond?: BondConfig | null
  has_password?: boolean
  subnet_id?: string
  vlan_override?: number
  nic_selector_override?: string
  updated_at?: string
}

// ---------- bmc ----------

export interface BmcCredential {
  machine_uuid: string
  name?: string
  ip: string
  port?: number
  username: string
  ipmi_interface?: string
  created_at?: string
  updated_at?: string
}

export interface BmcTestResult {
  ok: boolean
  power?: string
  error?: string
}

export interface BmcActionResult {
  ok: boolean
  action?: string
  error?: string
}

// ---------- jobs ----------

export type JobStatus = 'pending' | 'running' | 'succeeded' | 'failed' | 'cancelled'
export type JobType = 'install' | 'reinstall'

export interface Job {
  id: string
  machine_uuid: string
  type: JobType
  image_id?: string
  profile_id?: string
  status: JobStatus
  stage?: string
  error?: string
  created_at: string
  started_at?: string
  finished_at?: string
  created_by?: string
  retry_of_job_id?: string
}

export interface JobLog {
  id: number
  job_id: string
  ts: string
  level: 'debug' | 'info' | 'warn' | 'error'
  message: string
}

// ---------- settings ----------

export interface DhcpSettings {
  mode?: 'proxy' | 'full'
  interface?: string
  listen?: string
  // full 模式相关字段以宽松类型承接，具体键名以后端返回为准
  [key: string]: unknown
}

// ---------- audit ----------

export interface AuditEvent {
  id: number
  ts: string
  actor: string
  action: string
  target?: string
  outcome: 'ok' | 'failed'
  details?: Record<string, unknown> | null
}

// ---------- auth / util ----------

export interface AuthUser {
  username: string
}

export interface ApiError {
  error: string
}
