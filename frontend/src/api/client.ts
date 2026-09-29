import type { ApiError } from './types'

/** API base，由 index.html 的 <meta name="metalkit-api-base"> 注入，与旧版机制一致。 */
export const API_BASE: string =
  document.querySelector<HTMLMetaElement>('meta[name="metalkit-api-base"]')?.content || '/api/v1'

export class HttpError extends Error {
  constructor(
    public status: number,
    public body: ApiError | null,
    message: string,
  ) {
    super(message)
    this.name = 'HttpError'
  }

  /** 后端 {"error": "..."} 里的消息，兜底用状态码文本。 */
  get apiMessage(): string {
    return this.body?.error || `HTTP ${this.status}`
  }
}

/** 401 时统一跳登录页（带上当前路径以便回跳）。已在登录页则不重复跳。 */
function redirectUnauthorized(): void {
  if (window.location.pathname.startsWith('/ui/login')) return
  const next = encodeURIComponent(window.location.pathname + window.location.search)
  window.location.assign(`/ui/login?next=${next}`)
}

async function request<T>(
  method: string,
  path: string,
  opts: { body?: unknown; raw?: boolean; signal?: AbortSignal } = {},
): Promise<T> {
  let resp: Response
  try {
    resp = await fetch(API_BASE + path, {
      method,
      credentials: 'same-origin',
      headers: opts.body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
      body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
      signal: opts.signal,
    })
  } catch (err) {
    if ((err as Error).name === 'AbortError') throw err
    throw new HttpError(0, null, `网络请求失败: ${(err as Error).message}`)
  }

  if (resp.status === 401) {
    redirectUnauthorized()
    // 后续 .then 链不应继续，抛错终止；调用方对 401 通常无需处理（页面即将跳转）。
    throw new HttpError(401, { error: 'unauthorized' }, '未登录')
  }

  if (resp.status === 204) {
    return null as T
  }

  const text = await resp.text()
  let parsed: unknown = null
  if (text) {
    try {
      parsed = JSON.parse(text)
    } catch {
      parsed = null
    }
  }

  if (!resp.ok) {
    const apiErr = parsed as ApiError | null
    throw new HttpError(resp.status, apiErr, apiErr?.error || `HTTP ${resp.status}`)
  }
  return parsed as T
}

export function apiGet<T>(path: string, signal?: AbortSignal): Promise<T> {
  return request<T>('GET', path, { signal })
}

export function apiSend<T>(
  method: 'POST' | 'PUT' | 'DELETE',
  path: string,
  body?: unknown,
): Promise<T> {
  return request<T>(method, path, { body })
}

/** 分块上传用的裸 PUT（body 是 ArrayBuffer，非 JSON）。 */
export async function apiPutRaw(
  path: string,
  data: ArrayBuffer,
  headers: Record<string, string> = {},
): Promise<Response> {
  return fetch(API_BASE + path, {
    method: 'PUT',
    credentials: 'same-origin',
    headers,
    body: data,
  })
}
