/**
 * 硬件信息导出：把一份 machine Report 序列化为 CSV 与 TXT 两种形态。
 *
 * CSV：每个硬件组一段（机器/内存/磁盘/网卡），段间空行 + 组标题行，
 * Excel/WPS 直接打开。TXT：键值对 + 表格化的紧凑清单，贴工单/邮件用。
 * 纯函数，便于单测；下载动作由调用方（Blob + a[download]）完成。
 */
import type { Report } from '@/api/types'

function csvField(v: unknown): string {
  const s = v === null || v === undefined ? '' : String(v)
  return /[",\n\r]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s
}

// CPU 型号：去重后的每颗型号（多路同型号只列出一次）
function cpuModel(r: Report): string {
  return [...new Set((r.cpu?.per_socket ?? []).map((s) => s.model).filter(Boolean))].join(' + ')
}

function csvRow(cells: unknown[]): string {
  return cells.map(csvField).join(',')
}

function cell(v: unknown): string {
  if (v === null || v === undefined || v === '') return '—'
  return String(v)
}

/** 硬件清单 → CSV 文本 */
export function hardwareToCsv(r: Report): string {
  const out: string[] = []
  const m = r.machine ?? {}
  const fwRaw = (r.firmware ?? {}) as {
    bios?: { vendor?: string; version?: string }
    uefi_mode?: boolean
    secure_boot?: boolean
    tpm?: string
  }
  const biosVer = [fwRaw.bios?.vendor, fwRaw.bios?.version].filter(Boolean).join(' ')

  out.push(csvRow(['机器信息']))
  out.push(csvRow(['厂商', '产品名', '序列号', 'SMBIOS UUID', 'CPU 架构', 'CPU 厂商', 'CPU 型号', '插槽数', '物理核', '逻辑核', 'BIOS', 'UEFI', 'Secure Boot', 'TPM']))
  out.push(csvRow([
    m.manufacturer, m.product_name, m.serial, m.smbios_uuid,
    r.cpu?.arch, r.cpu?.vendor, cpuModel(r), r.cpu?.sockets, r.cpu?.total_cores, r.cpu?.total_threads,
    biosVer, fwRaw.uefi_mode ? '是' : '否',
    fwRaw.secure_boot ? '启用' : '未启用', fwRaw.tpm,
  ]))
  out.push('')

  const dimms = r.memory?.dimms ?? []
  out.push(csvRow(['内存', , `共 ${dimms.length} 条`]))
  out.push(csvRow(['插槽', '容量 (GiB)', '频率 (MT/s)', '类型', '厂商']))
  for (const d of dimms) {
    const gib = d.size_bytes ? (d.size_bytes / 1024 ** 3).toFixed(0) : ''
    out.push(csvRow([d.slot, gib, d.speed_mt_s, d.type, d.manufacturer]))
  }
  out.push('')

  const disks = r.disks ?? []
  out.push(csvRow(['磁盘', , `共 ${disks.length} 块`]))
  out.push(csvRow(['设备', '类型', '容量 (GiB)', '型号', 'WWN', '路径']))
  for (const d of disks) {
    const gib = (d.size_bytes ?? 0) > 0 ? (d.size_bytes! / 1024 ** 3).toFixed(1) : ''
    out.push(csvRow([d.kname, d.type, gib, d.model, d.wwn, d.path]))
  }
  out.push('')

  const nics = r.nics ?? []
  out.push(csvRow(['网卡', , `共 ${nics.length} 个`]))
  out.push(csvRow(['接口', 'MAC', '速率 (Mb/s)', '驱动']))
  for (const n of nics) {
    out.push(csvRow([n.name, n.mac, n.speed_mbps, n.driver]))
  }
  return out.join('\n') + '\n'
}

/** 硬件清单 → 可读 TXT（贴工单/邮件） */
export function hardwareToTxt(r: Report, header: { serial?: string; location?: string; notes?: string } = {}): string {
  const L: string[] = []
  const m = r.machine ?? {}
  const fwRaw = (r.firmware ?? {}) as {
    bios?: { vendor?: string; version?: string }
    uefi_mode?: boolean
    secure_boot?: boolean
    tpm?: string
  }
  const biosVer = [fwRaw.bios?.vendor, fwRaw.bios?.version].filter(Boolean).join(' ')

  L.push(`metalkit 硬件清单`)
  L.push(`机　器: ${cell(header.serial || m.serial)}　${cell([m.manufacturer, m.product_name].filter(Boolean).join(' '))}`)
  if (header.location) L.push(`位　置: ${header.location}`)
  if (header.notes) L.push(`备　注: ${header.notes}`)
  L.push(`UUID:　 ${cell(m.smbios_uuid)}`)
  L.push(`采集于: ${cell(r.collected_at)}`)
  L.push('')

  L.push('── 机器/固件 ──')
  L.push(`CPU:　　 ${cell(cpuModel(r))}（${cell(r.cpu?.sockets)} 路 ${cell(r.cpu?.total_cores)} 核 ${cell(r.cpu?.total_threads)} 线程）`)
  L.push(`架构:　　 ${cell(r.cpu?.arch)}　厂商: ${cell(r.cpu?.vendor)}　BIOS: ${cell(biosVer)}`)
  L.push(`UEFI: ${fwRaw.uefi_mode ? '是' : '否'}　Secure Boot: ${fwRaw.secure_boot ? '启用' : '未启用'}　TPM: ${cell(fwRaw.tpm)}`)
  L.push('')

  const dimms = r.memory?.dimms ?? []
  L.push(`── 内存（${dimms.length} 条）──`)
  for (const d of dimms) {
    const gib = d.size_bytes ? `${(d.size_bytes / 1024 ** 3).toFixed(0)}G` : '?'
    L.push(`${cell(d.slot)}  ${gib}  ${cell(d.speed_mt_s)}MT/s  ${cell(d.type)}  ${cell(d.manufacturer)}`)
  }
  L.push('')

  const disks = r.disks ?? []
  L.push(`── 磁盘（${disks.length} 块）──`)
  for (const d of disks) {
    const gib = (d.size_bytes ?? 0) > 0 ? `${(d.size_bytes! / 1024 ** 3).toFixed(1)}G` : '?'
    L.push(`${cell(d.kname)}  ${gib}  ${cell(d.type)}  ${cell(d.model)}  ${cell(d.wwn)}`)
  }
  L.push('')

  const nics = r.nics ?? []
  L.push(`── 网卡（${nics.length} 个）──`)
  for (const n of nics) {
    const speed = n.speed_mbps ? `${n.speed_mbps}Mb/s` : '?'
    L.push(`${cell(n.name)}  ${cell(n.mac)}  ${speed}  ${cell(n.driver)}`)
  }
  return L.join('\n') + '\n'
}
