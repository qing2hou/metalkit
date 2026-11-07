import { describe, expect, it } from 'vitest'

import { MAX_HOSTNAME_LEN, validateHostname } from './hostname'

describe('validateHostname（与后端 validateHostname 同规则）', () => {
  it('合法主机名', () => {
    expect(validateHostname('node-007')).toBe('')
    expect(validateHostname('web01')).toBe('')
    expect(validateHostname('a')).toBe('')
    expect(validateHostname('node-007.prod.example.com')).toBe('')
    expect(validateHostname('0node')).toBe('')
  })
  it('留空 = 不覆盖，视为合法', () => {
    expect(validateHostname('')).toBe('')
    expect(validateHostname('   ')).toBe('')
  })
  it('首尾不能是 . 或 -', () => {
    expect(validateHostname('-leading')).not.toBe('')
    expect(validateHostname('trailing-')).not.toBe('')
    expect(validateHostname('.dotted')).not.toBe('')
    expect(validateHostname('dotted.')).not.toBe('')
  })
  it('非法字符', () => {
    expect(validateHostname('host!name')).not.toBe('')
    expect(validateHostname('host name')).not.toBe('')
    expect(validateHostname('host_name')).not.toBe('')
    expect(validateHostname('主机名')).not.toBe('')
  })
  it('连续点 / 空段', () => {
    expect(validateHostname('a..b')).not.toBe('')
    expect(validateHostname('a.-b')).toBe('') // 后端只查段长不查段首尾，这里保持一致
  })
  it('每段 ≤63', () => {
    const ok63 = 'a'.repeat(63)
    expect(validateHostname(ok63)).toBe('')
    expect(validateHostname('a'.repeat(64))).not.toBe('')
    expect(validateHostname(`${ok63}.${ok63}`)).toBe('')
  })
  it('总长 ≤253', () => {
    const label = 'a'.repeat(63)
    const at253 = [label, label, label, 'a'.repeat(61)].join('.') // 63*3+61+3 = 253
    expect(at253.length).toBe(MAX_HOSTNAME_LEN)
    expect(validateHostname(at253)).toBe('')
    expect(validateHostname(`${at253}a`)).not.toBe('')
  })
  it('占位符给出可操作的提示', () => {
    expect(validateHostname('node-{serial}')).toContain('占位符')
    expect(validateHostname('node-{uuid8}')).toContain('占位符')
  })
})
