/**
 * RFC4180 简版 CSV 解析（与旧版 bmc.js 一致）：
 * 支持双引号包裹、引号转义（""）、字段内换行。
 * 输出为行的数组；空行保留为空数组（调用方自行跳过）。
 */
export function parseCsv(text: string): string[][] {
  const rows: string[][] = []
  let row: string[] = []
  let field = ''
  let inQuotes = false

  for (let i = 0; i < text.length; i++) {
    const c = text[i]
    if (inQuotes) {
      if (c === '"') {
        if (text[i + 1] === '"') {
          field += '"'
          i++
        } else {
          inQuotes = false
        }
      } else {
        field += c
      }
      continue
    }
    if (c === '"') {
      inQuotes = true
    } else if (c === ',') {
      row.push(field)
      field = ''
    } else if (c === '\n') {
      row.push(field)
      rows.push(row)
      row = []
      field = ''
    } else if (c === '\r') {
      // 吞掉，等 \n
    } else {
      field += c
    }
  }
  // 结尾（无换行符收尾的最后一行）
  row.push(field)
  rows.push(row)
  // 末尾可能因结尾换行产生一个空行，去掉
  const last = rows[rows.length - 1]
  if (last.length === 1 && last[0] === '') rows.pop()
  return rows
}
