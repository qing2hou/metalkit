/**
 * 镜像文件名 → OS 家族识别（与旧版 images.js detectFromFilename 一致）。
 * 后端 internal/images/detect.go 是权威实现，这里做同款客户端预填。
 */

export interface DetectResult {
  family: string
  version?: string
  /** 目标架构："amd64" | "arm64"，无法识别为空 */
  arch?: string
}

const ARCH_RE = /(^|[-_.])(amd64|x86[_-]?64|arm64|aarch64)([-_.]|$)/i

/** 从文件名识别目标架构：x86_64/amd64 → amd64；arm64/aarch64 → arm64。 */
export function detectArchFromFilename(filename: string): string {
  const m = ARCH_RE.exec(filename)
  if (!m) return ''
  const token = m[2].toLowerCase()
  if (token === 'amd64' || token.startsWith('x86')) return 'amd64'
  return 'arm64'
}

export function detectFromFilename(filename: string): DetectResult | null {
  const f = filename.toLowerCase()
  // 家族归并与后端 internal/images/detect.go 对齐：Rocky/Alma/RHEL 都是
  // rhel 家族（rhel installer 代码路径），CentOS 7 单独 rhel7。前端若
  // 填 rocky/almalinux/centos 会被后端 family 一致性检查拒绝
  // ("family mismatch: looks like rhel but you supplied rocky")。
  if (/ubuntu/.test(f)) return { family: 'ubuntu', arch: detectArchFromFilename(filename) }
  if (/centos[-_.]?7/.test(f)) return { family: 'rhel7', arch: detectArchFromFilename(filename) }
  if (/centos/.test(f)) return { family: 'rhel', arch: detectArchFromFilename(filename) }
  if (/rocky/.test(f)) return { family: 'rhel', arch: detectArchFromFilename(filename) }
  if (/almalinux|alma/.test(f)) return { family: 'rhel', arch: detectArchFromFilename(filename) }
  if (/debian/.test(f)) return { family: 'debian', arch: detectArchFromFilename(filename) }
  if (/kylin/.test(f)) return { family: 'kylin', arch: detectArchFromFilename(filename) }
  if (/openeuler|open-euler/.test(f)) return { family: 'openeuler', arch: detectArchFromFilename(filename) }
  if (/opensuse/.test(f)) return { family: 'opensuse', arch: detectArchFromFilename(filename) }
  if (/rhel/.test(f)) return { family: 'rhel', arch: detectArchFromFilename(filename) }
  return null
}

/** 生成 24 位随机密码（crypto），与旧版 🎲 行为一致：大小写+数字+符号。 */
export function generatePassword(length = 24): string {
  const upper = 'ABCDEFGHJKLMNPQRSTUVWXYZ'
  const lower = 'abcdefghijkmnopqrstuvwxyz'
  const digits = '23456789'
  const symbols = '!@#$%^&*'
  const all = upper + lower + digits + symbols
  const bytes = new Uint32Array(length)
  crypto.getRandomValues(bytes)
  let out = ''
  for (let i = 0; i < length; i++) {
    out += all[bytes[i] % all.length]
  }
  return out
}
