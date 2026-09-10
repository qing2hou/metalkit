/**
 * 网络相关纯函数：IPv4/CIDR 校验、host-in-subnet 判断。
 * hostInSubnet 语义必须与后端 internal/subnets HostInSubnet 一致（IPv4 only）。
 */

/** 解析点分 IPv4 为 uint32；非法返回 null。 */
export function parseIPv4(s: string): number | null {
  const parts = s.trim().split('.')
  if (parts.length !== 4) return null
  let v = 0
  for (const p of parts) {
    if (!/^\d{1,3}$/.test(p)) return null
    const n = Number(p)
    if (n > 255) return null
    v = v * 256 + n
  }
  return v
}

export function isValidIPv4(s: string): boolean {
  return parseIPv4(s) !== null
}

/** 解析 CIDR（IPv4）；非法返回 null。 */
export function parseCIDR(cidr: string): { addr: number; prefix: number } | null {
  const idx = cidr.indexOf('/')
  if (idx < 0) return null
  const addr = parseIPv4(cidr.slice(0, idx))
  if (addr === null) return null
  const prefixStr = cidr.slice(idx + 1)
  if (!/^\d{1,2}$/.test(prefixStr)) return null
  const prefix = Number(prefixStr)
  if (prefix > 32) return null
  return { addr, prefix }
}

export function isValidCIDR(s: string): boolean {
  return parseCIDR(s) !== null
}

/** 主机地址是否落在子网内（与 Go 端 HostInSubnet 语义对齐）。 */
export function hostInSubnet(host: string, cidr: string): boolean {
  const net = parseCIDR(cidr)
  const ip = parseIPv4(host)
  if (!net || ip === null) return false
  if (net.prefix === 0) return true
  const mask = net.prefix === 32 ? 0xffffffff : (0xffffffff << (32 - net.prefix)) >>> 0
  return ((ip & mask) >>> 0) === ((net.addr & mask) >>> 0)
}

/** CIDR 的网络地址与广播地址（字符串形式）；非法返回 null。 */
export function subnetRange(cidr: string): { network: string; broadcast: string } | null {
  const net = parseCIDR(cidr)
  if (!net) return null
  const mask = net.prefix === 0 ? 0 : net.prefix === 32 ? 0xffffffff : (0xffffffff << (32 - net.prefix)) >>> 0
  const network = (net.addr & mask) >>> 0
  const broadcast = (network | (~mask >>> 0)) >>> 0
  return { network: intToIPv4(network), broadcast: intToIPv4(broadcast) }
}

function intToIPv4(v: number): string {
  return `${(v >>> 24) & 0xff}.${(v >>> 16) & 0xff}.${(v >>> 8) & 0xff}.${v & 0xff}`
}

/** MAC 地址规范化：接受 aa:bb:cc:dd:ee:ff / AA-BB-…，输出小写冒号分隔。 */
export function normalizeMac(s: string): string {
  return s.trim().toLowerCase().replace(/-/g, ':')
}

export function isValidMac(s: string): boolean {
  return /^([0-9a-f]{2}:){5}[0-9a-f]{2}$/.test(normalizeMac(s))
}
