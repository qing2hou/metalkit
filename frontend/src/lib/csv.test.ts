import { describe, expect, it } from 'vitest'

import { parseCsv } from './csv'

describe('parseCsv', () => {
  it('解析基本行', () => {
    expect(parseCsv('a,b,c')).toEqual([['a', 'b', 'c']])
  })

  it('解析多行（\\n 结尾不产生空行）', () => {
    expect(parseCsv('a,b\nc,d\n')).toEqual([
      ['a', 'b'],
      ['c', 'd'],
    ])
  })

  it('处理 \\r\\n', () => {
    expect(parseCsv('a,b\r\nc,d')).toEqual([
      ['a', 'b'],
      ['c', 'd'],
    ])
  })

  it('双引号包裹含逗号字段', () => {
    expect(parseCsv('"a,b",c')).toEqual([['a,b', 'c']])
  })

  it('引号转义 ""', () => {
    expect(parseCsv('"say ""hi""",x')).toEqual([['say "hi"', 'x']])
  })

  it('字段内换行', () => {
    const rows = parseCsv('"line1\nline2",b\nnext,row')
    expect(rows).toEqual([
      ['line1\nline2', 'b'],
      ['next', 'row'],
    ])
  })

  it('空字符串与全空行', () => {
    expect(parseCsv('')).toEqual([])
    expect(parseCsv('a,b\n\nc,d')).toEqual([
      ['a', 'b'],
      [''],
      ['c', 'd'],
    ])
  })
})
