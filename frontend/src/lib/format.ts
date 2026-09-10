/**
 * 时间/字节格式化工具，语义与旧版 common.js 保持一致（中文相对时间）。
 */

/** 相对时间：「3 秒前」「2 分钟前」等；超过 30 天回退为绝对时间。 */
export function fmtRelative(iso?: string | null): string {
  if (!iso) return '—'
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return '—'
  const diff = Date.now() - t
  if (diff < 0) return fmtAbsolute(iso)
  const s = Math.floor(diff / 1000)
  if (s < 5) return '刚刚'
  if (s < 60) return `${s} 秒前`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} 分钟前`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h} 小时前`
  const d = Math.floor(h / 24)
  if (d < 30) return `${d} 天前`
  return fmtAbsolute(iso)
}

/** 绝对时间：YYYY-MM-DD HH:mm（本地时区）。 */
export function fmtAbsolute(iso?: string | null): string {
  if (!iso) return '—'
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return '—'
  const d = new Date(t)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

/** ISO 字符串原样输出（无效时占位）。 */
export function fmtISO(iso?: string | null): string {
  if (!iso) return '—'
  if (Number.isNaN(Date.parse(iso))) return '—'
  return iso
}

/** 时长（毫秒）：→「3.2 秒」「2 分 14 秒」「1 时 3 分」。 */
export function fmtDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '—'
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${(ms / 1000).toFixed(1)} 秒`
  const m = Math.floor(s / 60)
  if (m < 60) {
    const rs = s % 60
    return rs ? `${m} 分 ${rs} 秒` : `${m} 分`
  }
  const h = Math.floor(m / 60)
  const rm = m % 60
  return rm ? `${h} 时 ${rm} 分` : `${h} 时`
}

const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']

/** 字节数：二进制单位，保留一位小数（不足 KiB 显示整数）。 */
export function fmtBytes(bytes?: number | null): string {
  if (bytes === undefined || bytes === null || !Number.isFinite(bytes)) return '—'
  if (bytes === 0) return '0 B'
  let v = bytes
  let i = 0
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${UNITS[i]}`
}

/** 状态 → 语义色（Element Plus tag type），未知状态为 info。 */
export function statusTagType(status: string): 'success' | 'warning' | 'danger' | 'info' | 'primary' {
  switch (status) {
    case 'online':
    case 'succeeded':
    case 'ok':
      return 'success'
    case 'pending':
      return 'warning'
    case 'offline':
    case 'failed':
      return 'danger'
    case 'running':
      return 'primary'
    default:
      return 'info'
  }
}

/** 状态中文化。 */
export function statusLabel(status?: string | null): string {
  const map: Record<string, string> = {
    online: '在线',
    offline: '离线',
    pending: '等待中',
    running: '运行中',
    succeeded: '已成功',
    failed: '已失败',
    cancelled: '已取消',
    live: 'Live 系统',
    unknown: '未知',
  }
  if (!status) return '—'
  return map[status] ?? status
}

/** 日志级别 → 颜色 class。 */
export function logLevelClass(level: string): string {
  switch (level) {
    case 'error':
      return 'log-error'
    case 'warn':
      return 'log-warn'
    case 'debug':
      return 'log-debug'
    default:
      return 'log-info'
  }
}
