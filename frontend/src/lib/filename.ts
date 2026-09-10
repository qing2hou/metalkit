/**
 * 镜像文件名 → OS 家族识别（与旧版 images.js detectFromFilename 一致）。
 * 后端 internal/images/detect.go 是权威实现，这里做同款客户端预填。
 */

export interface FamilyGuess {
  family: string
  version?: string
}

export function detectFromFilename(filename: string): FamilyGuess | null {
  const f = filename.toLowerCase()
  if (/ubuntu/.test(f)) return { family: 'ubuntu' }
  if (/centos/.test(f)) return { family: 'centos' }
  if (/rocky/.test(f)) return { family: 'rocky' }
  if (/almalinux/.test(f)) return { family: 'almalinux' }
  if (/debian/.test(f)) return { family: 'debian' }
  if (/kylin/.test(f)) return { family: 'kylin' }
  if (/openeuler|open-euler/.test(f)) return { family: 'openeuler' }
  if (/opensuse/.test(f)) return { family: 'opensuse' }
  if (/rhel/.test(f)) return { family: 'rhel' }
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
