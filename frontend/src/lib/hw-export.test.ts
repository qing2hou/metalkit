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
  cpu: { arch: 'amd64', vendor: 'GenuineIntel', sockets: 2, total_cores: 20, total_threads: 40, per_socket: [{ socket: 'CPU1', model: 'Intel(R) Xeon(R) CPU E5-2630 v4 @ 2.20GHz', cores: 10, threads: 20 }] },
  memory: {
    total_bytes: 64 * 1024 ** 3,
    dimms: [
      {
        locator: 'DIMM_A1', size_bytes: 32 * 1024 ** 3, speed_mts: 2400, configured_speed_mts: 2133,
        type: 'DDR4', manufacturer: 'Samsung', part_number: 'M393A4K40DB1-CVF', serial: '3582D1AF', rank: 2,
      },
    ],
  },
  disks: [
    { kname: 'sda', path: '/dev/sda', type: 'ssd', size_bytes: 480_000_000_000, model: 'Dell SSD 480GB', serial: 'S6XENA0R409123', wwn: '0x5000c500a1' },
  ],
  nics: [
    { name: 'eno1', mac: 'aa:bb:cc:dd:ee:ff', speed_mbps: 1000, driver: 'tg3' },
    { name: 'eno2np0', mac: 'aa:bb:cc:dd:ee:01', speed_mbps: 25000, driver: 'mlx5_core', sfp: { vendor: 'HG GENUINE', part_number: 'MTRA-3E31A-01', serial: '7000282830002' } },
  ],
}

describe('hardwareToCsv', () => {
  it('包含全部四个硬件段与字段', () => {
    const csv = hardwareToCsv(sample)
    expect(csv).toContain('机器信息')
    expect(csv).toContain('Intel(R) Xeon(R) CPU E5-2630 v4 @ 2.20GHz')
    expect(csv).toContain('93MPG82')
    expect(csv).toContain('内存')
    expect(csv).toContain('插槽,容量 (GiB),频率 (MT/s),类型,厂商,PN,序列号,Rank')
    expect(csv).toContain('DIMM_A1,32,2133,DDR4,Samsung,M393A4K40DB1-CVF,3582D1AF,2')
    expect(csv).toContain('磁盘')
    expect(csv).toContain('sda,ssd,447.0,Dell SSD 480GB,S6XENA0R409123,0x5000c500a1')
    expect(csv).toContain('网卡')
    expect(csv).toContain('接口,MAC,速率 (Mb/s),驱动,光模块厂商,光模块 PN,光模块 SN')
    expect(csv).toContain('eno1,aa:bb:cc:dd:ee:ff,1000,tg3,,,')
    expect(csv).toContain('HG GENUINE,MTRA-3E31A-01,7000282830002')
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
    expect(txt).toContain('CPU:　　 Intel(R) Xeon(R) CPU E5-2630 v4 @ 2.20GHz（2 路 20 核 40 线程）')
    expect(txt).toContain('── 内存（1 条）──')
    expect(txt).toContain('DIMM_A1  32G  2133MT/s  DDR4  Samsung  PN M393A4K40DB1-CVF  SN 3582D1AF  R2')
    expect(txt).toContain('── 磁盘（1 块）──')
    expect(txt).toContain('SN S6XENA0R409123')
    expect(txt).toContain('── 网卡（2 个）──')
    expect(txt).toContain('eno1  aa:bb:cc:dd:ee:ff  1000Mb/s  tg3')
    expect(txt).toContain('SFP: HG GENUINE / MTRA-3E31A-01 / 7000282830002')
  })

  it('无位置/备注时头两行省略且不出现 —', () => {
    const txt = hardwareToTxt(sample)
    expect(txt).not.toContain('位　置:')
    expect(txt).not.toContain('备　注:')
  })
})
