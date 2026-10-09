import { describe, expect, it } from 'vitest'

import type { Report } from '@/api/types'

import { hardwareToCsv, hardwareToTxt } from './hw-export'

const sample: Report = {
  collected_at: '2026-10-09T05:00:00Z',
  machine: {
    smbios_uuid: '4c4c4544-0033-4d10-8050-b9c04f473832',
    manufacturer: 'Dell Inc.',
    product_name: 'PowerEdge R630',
    serial: '93MPG82',
  },
  firmware: { bios: { vendor: 'Dell Inc.', version: '2.11.4' }, uefi_mode: true, secure_boot: false },
  cpu: { arch: 'amd64' },
  memory: {
    total_bytes: 64 * 1024 ** 3,
    dimms: [{ slot: 'A1', size_bytes: 32 * 1024 ** 3, speed_mt_s: 2400, type: 'DDR4', manufacturer: 'Samsung' }],
  },
  disks: [
    { kname: 'sda', path: '/dev/sda', type: 'ssd', size_bytes: 480_000_000_000, model: 'Dell SSD 480GB', wwn: '0x5000c500a1' },
  ],
  nics: [{ name: 'eno1', mac: 'aa:bb:cc:dd:ee:ff', speed_mbps: 1000, driver: 'tg3' }],
}

describe('hardwareToCsv', () => {
  it('包含全部四个硬件段与字段', () => {
    const csv = hardwareToCsv(sample)
    expect(csv).toContain('机器信息')
    expect(csv).toContain('93MPG82')
    expect(csv).toContain('内存')
    expect(csv).toContain('A1,32,2400,DDR4,Samsung')
    expect(csv).toContain('磁盘')
    expect(csv).toContain('sda,ssd,447.0,Dell SSD 480GB,0x5000c500a1')
    expect(csv).toContain('网卡')
    expect(csv).toContain('eno1,aa:bb:cc:dd:ee:ff,1000,tg3')
  })

  it('字段含逗号/引号时正确转义（RFC4180）', () => {
    const odd = structuredClone(sample)
    odd.machine!.manufacturer = 'Dell, Inc. "Enterprise"'
    const csv = hardwareToCsv(odd)
    expect(csv).toContain('"Dell, Inc. ""Enterprise"""')
  })

  it('空硬件组也能导出（表头在、行为空）', () => {
    const empty = structuredClone(sample)
    empty.disks = []
    const csv = hardwareToCsv(empty)
    expect(csv).toContain('磁盘')
    expect(csv).not.toContain('sda')
  })
})

describe('hardwareToTxt', () => {
  it('带位置/备注头 + 硬件分组齐全', () => {
    const txt = hardwareToTxt(sample, { serial: '93MPG82', location: '机房2-R05-U12', notes: '数据库节点' })
    expect(txt).toContain('metalkit 硬件清单')
    expect(txt).toContain('位　置: 机房2-R05-U12')
    expect(txt).toContain('备　注: 数据库节点')
    expect(txt).toContain('── 内存（1 条）──')
    expect(txt).toContain('A1  32G  2400MT/s  DDR4  Samsung')
    expect(txt).toContain('── 磁盘（1 块）──')
    expect(txt).toContain('── 网卡（1 个）──')
    expect(txt).toContain('eno1  aa:bb:cc:dd:ee:ff  1000Mb/s  tg3')
  })

  it('无位置/备注时头两行省略且不出现 —', () => {
    const txt = hardwareToTxt(sample)
    expect(txt).not.toContain('位　置:')
    expect(txt).not.toContain('备　注:')
  })
})
