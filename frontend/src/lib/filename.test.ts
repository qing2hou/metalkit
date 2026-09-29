import { describe, expect, it } from 'vitest'

import { detectFromFilename, generatePassword } from './filename'
import { fmtBytes, fmtDuration, fmtRelative } from './format'

describe('detectFromFilename', () => {
  it('识别常见发行版（家族与后端 detect.go 对齐：rocky/alma/centos→rhel）', () => {
    expect(detectFromFilename('ubuntu-22.04-server.qcow2')?.family).toBe('ubuntu')
    expect(detectFromFilename('CentOS-7-x86_64.qcow2')?.family).toBe('rhel7')
    expect(detectFromFilename('Rocky-8-GenericCloud.latest.x86_64.qcow2')?.family).toBe('rhel')
    expect(detectFromFilename('rocky-9.img')?.family).toBe('rhel')
    expect(detectFromFilename('AlmaLinux-9-GenericCloud.x86_64.qcow2')?.family).toBe('rhel')
    expect(detectFromFilename('Kylin-V10.img')?.family).toBe('kylin')
    expect(detectFromFilename('openEuler-22.03.qcow2')?.family).toBe('openeuler')
  })
  it('未知返回 null', () => {
    expect(detectFromFilename('mydisk.qcow2')).toBeNull()
  })
})

describe('generatePassword', () => {
  it('长度正确且字符集符合', () => {
    const pwd = generatePassword(24)
    expect(pwd).toHaveLength(24)
    expect(pwd).toMatch(/^[A-Za-z0-9!@#$%^&*]+$/)
  })
  it('两次生成不同', () => {
    expect(generatePassword(24)).not.toBe(generatePassword(24))
  })
})

describe('fmtRelative / fmtDuration / fmtBytes', () => {
  it('相对时间', () => {
    const now = new Date().toISOString()
    expect(fmtRelative(now)).toBe('刚刚')
    expect(fmtRelative(new Date(Date.now() - 30_000).toISOString())).toBe('30 秒前')
    expect(fmtRelative(new Date(Date.now() - 5 * 60_000).toISOString())).toBe('5 分钟前')
    expect(fmtRelative(null)).toBe('—')
  })
  it('时长', () => {
    expect(fmtDuration(1500)).toBe('1.5 秒')
    expect(fmtDuration(134_000)).toBe('2 分 14 秒')
    expect(fmtDuration(3_600_000)).toBe('1 时')
  })
  it('字节', () => {
    expect(fmtBytes(0)).toBe('0 B')
    expect(fmtBytes(512)).toBe('512 B')
    expect(fmtBytes(1024)).toBe('1.0 KiB')
    expect(fmtBytes(1024 * 1024 * 1024)).toBe('1.0 GiB')
    expect(fmtBytes(null)).toBe('—')
  })
})
