/**
 * 自定义主机名校验（装机弹窗用）。
 *
 * 规则逐条对齐后端 internal/bindings/validate.go 的 validateHostname：
 * trim → 总长 ≤253（RFC-1035 §2.3.4）→ RFC-1123 字符集 →
 * 每段（点分）1–63 字符。后端是权威实现，这里只是让用户在提交前
 * 就拿到可读的中文提示；两边不一致时以后端 400 为准。
 */

/** 与 validate.go 的 MaxHostnameLen 一致。 */
export const MAX_HOSTNAME_LEN = 253

// 后端 hostnameRE：首尾必须是字母数字，中间可含 . 与 -
const HOSTNAME_RE = /^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$/

/**
 * 合法返回空串，否则返回中文错误。空输入视为合法（= 不覆盖，
 * 回落到 profile.hostname_template）。
 */
export function validateHostname(s: string): string {
  const h = s.trim()
  if (h === '') return ''
  if (h.includes('{') || h.includes('}')) {
    // 后端正则同样拒绝，但错误信息是 "invalid characters"，对用户没有指向性
    return '自定义主机名不支持 {serial} 这类占位符；请在 Profile 的主机名模板里使用'
  }
  if (h.length > MAX_HOSTNAME_LEN) {
    return `主机名最长 ${MAX_HOSTNAME_LEN} 个字符（当前 ${h.length}）`
  }
  if (!HOSTNAME_RE.test(h)) {
    return '仅允许字母、数字、. 与 -，且首尾必须是字母或数字'
  }
  for (const label of h.split('.')) {
    if (label.length === 0 || label.length > 63) {
      return '点分的每一段必须是 1–63 个字符'
    }
  }
  return ''
}
