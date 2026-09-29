import { describe, expect, it } from 'vitest'

import { hostInSubnet, isValidCIDR, isValidIPv4, isValidMac, subnetRange } from './net'

describe('IPv4 校验', () => {
  it('合法地址', () => {
    expect(isValidIPv4('192.168.1.1')).toBe(true)
    expect(isValidIPv4('0.0.0.0')).toBe(true)
    expect(isValidIPv4('255.255.255.255')).toBe(true)
  })
  it('非法地址', () => {
    expect(isValidIPv4('256.1.1.1')).toBe(false)
    expect(isValidIPv4('1.2.3')).toBe(false)
    expect(isValidIPv4('a.b.c.d')).toBe(false)
    expect(isValidIPv4('1.2.3.4.5')).toBe(false)
    expect(isValidIPv4('')).toBe(false)
  })
})

describe('CIDR 校验', () => {
  it('合法 CIDR', () => {
    expect(isValidCIDR('192.168.0.0/24')).toBe(true)
    expect(isValidCIDR('10.0.0.0/8')).toBe(true)
    expect(isValidCIDR('1.2.3.4/32')).toBe(true)
    expect(isValidCIDR('0.0.0.0/0')).toBe(true)
  })
  it('非法 CIDR', () => {
    expect(isValidCIDR('192.168.0.0')).toBe(false)
    expect(isValidCIDR('192.168.0.0/33')).toBe(false)
    expect(isValidCIDR('300.0.0.0/8')).toBe(false)
    expect(isValidCIDR('192.168.0.0/')).toBe(false)
  })
})

describe('hostInSubnet（与 Go HostInSubnet 语义对齐）', () => {
  it('同 /24 子网', () => {
    expect(hostInSubnet('192.168.1.10', '192.168.1.0/24')).toBe(true)
    expect(hostInSubnet('192.168.1.255', '192.168.1.0/24')).toBe(true)
    expect(hostInSubnet('192.168.2.10', '192.168.1.0/24')).toBe(false)
  })
  it('非边界网络地址也应成立（按掩码比较）', () => {
    expect(hostInSubnet('10.1.2.3', '10.1.2.9/24')).toBe(true)
  })
  it('/16 与 /8', () => {
    expect(hostInSubnet('10.20.30.40', '10.20.0.0/16')).toBe(true)
    expect(hostInSubnet('10.21.30.40', '10.20.0.0/16')).toBe(false)
    expect(hostInSubnet('10.99.0.1', '10.0.0.0/8')).toBe(true)
    expect(hostInSubnet('11.0.0.1', '10.0.0.0/8')).toBe(false)
  })
  it('/32 精确匹配', () => {
    expect(hostInSubnet('1.2.3.4', '1.2.3.4/32')).toBe(true)
    expect(hostInSubnet('1.2.3.5', '1.2.3.4/32')).toBe(false)
  })
  it('/0 匹配一切', () => {
    expect(hostInSubnet('8.8.8.8', '0.0.0.0/0')).toBe(true)
  })
  it('非法输入', () => {
    expect(hostInSubnet('not-an-ip', '10.0.0.0/8')).toBe(false)
    expect(hostInSubnet('10.0.0.1', 'not-a-cidr')).toBe(false)
  })
})

describe('subnetRange', () => {
  it('/24', () => {
    expect(subnetRange('192.168.1.0/24')).toEqual({
      network: '192.168.1.0',
      broadcast: '192.168.1.255',
    })
  })
  it('/30', () => {
    expect(subnetRange('10.0.0.4/30')).toEqual({ network: '10.0.0.4', broadcast: '10.0.0.7' })
  })
  it('非法返回 null', () => {
    expect(subnetRange('nope')).toBeNull()
  })
})

describe('MAC 校验', () => {
  it('合法（冒号与横线、大小写）', () => {
    expect(isValidMac('aa:bb:cc:dd:ee:ff')).toBe(true)
    expect(isValidMac('AA-BB-CC-DD-EE-FF')).toBe(true)
  })
  it('非法', () => {
    expect(isValidMac('aa:bb:cc:dd:ee')).toBe(false)
    expect(isValidMac('gg:bb:cc:dd:ee:ff')).toBe(false)
  })
})
