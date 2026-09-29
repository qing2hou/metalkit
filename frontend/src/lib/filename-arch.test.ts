import { describe, expect, it } from 'vitest'

import { detectArchFromFilename, detectFromFilename } from './filename'

describe('detectArchFromFilename（与后端 detect.go 同规则）', () => {
  it('识别 amd64 形态', () => {
    expect(detectArchFromFilename('jammy-server-cloudimg-amd64.img')).toBe('amd64')
    expect(detectArchFromFilename('centos-7-x86_64-GenericCloud.qcow2')).toBe('amd64')
    expect(detectArchFromFilename('debian-12-x86-64.qcow2')).toBe('amd64')
  })
  it('识别 arm64 形态', () => {
    expect(detectArchFromFilename('ubuntu-22.04-server-cloudimg-arm64.img')).toBe('arm64')
    expect(detectArchFromFilename('Rocky-9-aarch64.qcow2')).toBe('arm64')
  })
  it('无标记为空', () => {
    expect(detectArchFromFilename('mydisk.qcow2')).toBe('')
    expect(detectArchFromFilename('somediskamd64.raw')).toBe('') // 无分隔符不匹配
  })
  it('detectFromFilename 返回结构带 arch', () => {
    const d = detectFromFilename('ubuntu-22.04-server-cloudimg-arm64.img')
    expect(d?.family).toBe('ubuntu')
    expect(d?.arch).toBe('arm64')
  })
})
