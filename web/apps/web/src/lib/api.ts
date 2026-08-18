export type Credentials = {
  apiUrl: string
  token: string
}

export type AccountStats = {
  account_id?: string
  email: string
  file_path?: string
  status: string
  plan_type?: string
  has_refresh_token?: boolean
  disable_reason?: string
  total_requests: number
  total_errors: number
  consecutive_failures: number
  last_used_at?: string
  last_refreshed_at?: string
  cooldown_until?: string
  quota_exhausted: boolean
  quota_resets_at?: string
  token_expire?: string
  usage: {
    total_completions: number
    input_tokens: number
    output_tokens: number
    total_tokens: number
    cache_read_tokens: number
    cache_write_tokens: number
    reasoning_tokens: number
  }
  quota?: {
    valid: boolean
    status_code: number
    raw_data?: unknown
    checked_at: string
  }
}

export type CatalogStatus = {
  client_version: string
  revision: number
  model_count: number
  updated_at?: string
  source?: string
  refresh_interval_sec: number
  last_checked_at?: string
  last_error?: string
}

export type StatsResponse = {
  summary: {
    total: number
    active: number
    cooldown: number
    disabled: number
    rpm: number
    total_completions: number
    total_input_tokens: number
    total_output_tokens: number
    total_cache_read_tokens: number
    total_cache_write_tokens: number
    total_reasoning_tokens: number
    total_tokens: number
    quota_checked: number
    quota_valid: number
    quota_invalid: number
    quota_exhausted: number
  }
  catalog: CatalogStatus
  accounts: AccountStats[]
  pagination?: {
    page: number
    page_size: number
    total: number
    filtered_total: number
    total_pages: number
    returned: number
    has_prev: boolean
    has_next: boolean
    query?: string
  }
}

export type StatsParams = {
  page: number
  pageSize: number
  query: string
  includeQuota?: boolean
}

export type ProgressEvent = {
  type: string
  email?: string
  message?: string
  success?: boolean
  current?: number
  total?: number
  success_count?: number
  failed_count?: number
  remaining?: number
  duration?: string
}

export type IngestResult = {
  added: number
  updated: number
  failed: number
  pool_total: number
  errors?: string[]
}

export type AccountPatch = {
  email?: string
  account_id?: string
  id_token?: string
  access_token?: string
  refresh_token?: string
  rk?: string
  expired?: string
  plan_type?: string
  status?: string
  disable_reason?: string
  cooldown_until?: string
}

export type AccountMutationResult = {
  object: string
  account: AccountStats
}

export type AccountDeleteResult = {
  object: string
  email: string
  file_path?: string
  hard: boolean
}

export type AccountProbeResult = {
  object: string
  email: string
  file_path?: string
  status: "ok" | "rate_limited" | "invalid" | "transient_failed" | string
  verdict: number
  http_status: number
  message: string
  checked_at: string
  quota?: AccountStats["quota"]
}

export const CREDENTIALS_KEY = "codex_proxy_credentials_v1"

export function defaultCredentials(): Credentials {
  return {
    apiUrl: `${window.location.origin}/stats`,
    token: "",
  }
}

export function loadCredentials(): Credentials {
  const fallback = defaultCredentials()
  const raw = window.localStorage.getItem(CREDENTIALS_KEY)
  if (!raw) return fallback
  try {
    return { ...fallback, ...JSON.parse(raw) }
  } catch {
    return fallback
  }
}

export function saveCredentials(credentials: Credentials) {
  window.localStorage.setItem(CREDENTIALS_KEY, JSON.stringify(credentials))
}

function authHeaders(credentials: Credentials) {
  const headers: Record<string, string> = {}
  if (credentials.token.trim()) {
    headers.Authorization = `Bearer ${credentials.token.trim()}`
  }
  return headers
}

function endpointUrl(credentials: Credentials, endpoint: string) {
  const url = new URL(credentials.apiUrl || "/stats", window.location.href)
  const normalizedEndpoint = endpoint.startsWith("/") ? endpoint : `/${endpoint}`
  const basePath = url.pathname.replace(/\/stats\/?$/, "").replace(/\/$/, "")
  url.pathname = `${basePath}${normalizedEndpoint}`
  url.search = ""
  return url
}

function statsUrl(credentials: Credentials, params: StatsParams) {
  const url = new URL(credentials.apiUrl || "/stats", window.location.href)
  url.searchParams.set("page", String(params.page))
  url.searchParams.set("page_size", String(params.pageSize))
  url.searchParams.set("include_quota", params.includeQuota === false ? "0" : "1")
  if (params.query.trim()) {
    url.searchParams.set("q", params.query.trim())
  } else {
    url.searchParams.delete("q")
  }
  return url
}

export class RequestError extends Error {
  status: number
  code: string

  constructor(message: string, status: number, code = "") {
    super(message)
    this.status = status
    this.code = code
  }
}

async function responseError(response: Response) {
  let message = `HTTP ${response.status}`
  let code = ""
  try {
    const payload = await response.clone().json()
    message = payload?.error?.message || payload?.message || message
    code = payload?.error?.code || payload?.code || ""
  } catch {
    const text = (await response.text()).trim()
    if (text) message = text
  }
  return new RequestError(message, response.status, code)
}

export function isAuthError(error: unknown) {
  return (
    error instanceof RequestError &&
    (error.status === 401 ||
      error.status === 403 ||
      error.code === "invalid_api_key")
  )
}

export async function fetchStats(
  credentials: Credentials,
  params: StatsParams,
  signal?: AbortSignal
) {
  const response = await fetch(statsUrl(credentials, params), {
    headers: authHeaders(credentials),
    signal,
  })
  if (!response.ok) throw await responseError(response)
  return (await response.json()) as StatsResponse
}

export async function refreshCatalog(
  credentials: Credentials,
  signal?: AbortSignal
) {
  const response = await fetch(endpointUrl(credentials, "/admin/catalog/refresh"), {
    method: "POST",
    headers: authHeaders(credentials),
    signal,
  })
  if (!response.ok) throw await responseError(response)
  return (await response.json()) as { catalog: CatalogStatus }
}

export async function runProgressStream(
  credentials: Credentials,
  endpoint: "/check-quota" | "/refresh",
  onEvent: (event: ProgressEvent) => void,
  signal?: AbortSignal
) {
  const response = await fetch(endpointUrl(credentials, endpoint), {
    method: "POST",
    headers: authHeaders(credentials),
    signal,
  })
  if (!response.ok) throw await responseError(response)
  if (!response.body) return

  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ""
  const dispatchChunk = (chunk: string) => {
    const data = chunk
      .split("\n")
      .filter((line) => line.startsWith("data:"))
      .map((line) => line.slice(5).trim())
      .join("\n")
      .trim()

    if (!data) return

    try {
      onEvent(JSON.parse(data) as ProgressEvent)
    } catch {
      return
    }
  }

  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    const chunks = buffer.split("\n\n")
    buffer = chunks.pop() ?? ""
    for (const chunk of chunks) {
      dispatchChunk(chunk)
    }
  }

  buffer += decoder.decode()
  if (buffer.trim()) {
    dispatchChunk(buffer)
  }
}

export async function ingestAccounts(
  credentials: Credentials,
  body: string,
  contentType: "application/json" | "application/x-ndjson" = "application/json",
  signal?: AbortSignal
) {
  const response = await fetch(endpointUrl(credentials, "/admin/accounts/ingest"), {
    method: "POST",
    headers: {
      ...authHeaders(credentials),
      "Content-Type": contentType,
    },
    body,
    signal,
  })

  if (!response.ok) throw await responseError(response)
  return (await response.json()) as IngestResult
}

export async function createAccount(
  credentials: Credentials,
  account: AccountPatch,
  signal?: AbortSignal
) {
  const response = await fetch(endpointUrl(credentials, "/admin/accounts"), {
    method: "POST",
    headers: {
      ...authHeaders(credentials),
      "Content-Type": "application/json",
    },
    body: JSON.stringify(account),
    signal,
  })

  if (!response.ok) throw await responseError(response)
  return (await response.json()) as IngestResult
}

export async function updateAccount(
  credentials: Credentials,
  target: Pick<AccountStats, "email" | "file_path">,
  account: AccountPatch,
  signal?: AbortSignal
) {
  const response = await fetch(endpointUrl(credentials, "/admin/accounts"), {
    method: "PUT",
    headers: {
      ...authHeaders(credentials),
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      email: target.email,
      file_path: target.file_path,
      account,
    }),
    signal,
  })

  if (!response.ok) throw await responseError(response)
  return (await response.json()) as AccountMutationResult
}

export async function deleteAccount(
  credentials: Credentials,
  target: Pick<AccountStats, "email" | "file_path">,
  hard = true,
  signal?: AbortSignal
) {
  const response = await fetch(endpointUrl(credentials, "/admin/accounts"), {
    method: "DELETE",
    headers: {
      ...authHeaders(credentials),
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      email: target.email,
      file_path: target.file_path,
      hard,
    }),
    signal,
  })

  if (!response.ok) throw await responseError(response)
  return (await response.json()) as AccountDeleteResult
}

export async function probeAccount(
  credentials: Credentials,
  target: Pick<AccountStats, "email" | "file_path">,
  signal?: AbortSignal
) {
  const response = await fetch(
    endpointUrl(credentials, "/admin/accounts/probe"),
    {
      method: "POST",
      headers: {
        ...authHeaders(credentials),
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        email: target.email,
        file_path: target.file_path,
      }),
      signal,
    }
  )

  if (!response.ok) throw await responseError(response)
  return (await response.json()) as AccountProbeResult
}
