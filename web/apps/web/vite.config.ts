import path from "path"
import type { IncomingMessage, ServerResponse } from "http"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

type Middleware = (
  req: IncomingMessage,
  res: ServerResponse,
  next: () => void
) => void

type MockAccount = {
  account_id: string
  email: string
  file_path: string
  status: string
  plan_type: string
  has_refresh_token: boolean
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
  }
  quota?: {
    valid: boolean
    status_code: number
    checked_at: string
    raw_data?: unknown
  }
}

const now = Date.now()
const hours = (value: number) => new Date(now + value * 60 * 60 * 1000).toISOString()
const days = (value: number) => new Date(now + value * 24 * 60 * 60 * 1000).toISOString()

const mockAccounts: MockAccount[] = [
  {
    account_id: "acct_alpha_plus",
    email: "alpha.plus@example.test",
    file_path: "auths/alpha.plus@example.test.json",
    status: "active",
    plan_type: "plus",
    has_refresh_token: true,
    total_requests: 18420,
    total_errors: 12,
    consecutive_failures: 0,
    last_used_at: hours(-1),
    last_refreshed_at: hours(-7),
    quota_exhausted: false,
    token_expire: days(19),
    usage: {
      total_completions: 621,
      input_tokens: 1824000,
      output_tokens: 713200,
      total_tokens: 2537200,
    },
    quota: {
      valid: true,
      status_code: 200,
      checked_at: hours(-0.2),
      raw_data: {
        plan: "plus",
        rate_limit: {
          rolling_window: {
            duration_seconds: 18000,
            used: 38,
            limit: 80,
            remaining: 42,
            reset_at: hours(2.4),
          },
          weekly_window: {
            duration: "7 days",
            used_percent: 41,
            remaining: 590,
            reset_at: days(3),
          },
        },
      },
    },
  },
  {
    account_id: "acct_beta_pro",
    email: "beta.pro@example.test",
    file_path: "auths/beta.pro@example.test.json",
    status: "active",
    plan_type: "pro",
    has_refresh_token: true,
    total_requests: 49210,
    total_errors: 31,
    consecutive_failures: 0,
    last_used_at: hours(-0.1),
    last_refreshed_at: hours(-2),
    quota_exhausted: false,
    token_expire: days(27),
    usage: {
      total_completions: 1402,
      input_tokens: 4820000,
      output_tokens: 2210000,
      total_tokens: 7030000,
    },
    quota: {
      valid: true,
      status_code: 200,
      checked_at: hours(-0.1),
      raw_data: {
        subscription: "pro",
        limits: [
          {
            window: "3 hours",
            usedPercent: 62,
            remaining: 114,
            resetsAt: hours(1.7),
          },
          {
            period: "7d",
            current: 1740,
            max: 5000,
            available: 3260,
            reset_time: days(5),
          },
        ],
      },
    },
  },
  {
    account_id: "acct_gamma_team",
    email: "gamma-team@example.test",
    file_path: "auths/gamma-team@example.test.json",
    status: "cooldown",
    plan_type: "team",
    has_refresh_token: true,
    disable_reason: "rate limited by upstream",
    total_requests: 36480,
    total_errors: 88,
    consecutive_failures: 2,
    last_used_at: hours(-0.4),
    last_refreshed_at: hours(-11),
    cooldown_until: hours(0.8),
    quota_exhausted: false,
    token_expire: days(8),
    usage: {
      total_completions: 998,
      input_tokens: 2912000,
      output_tokens: 1189000,
      total_tokens: 4101000,
    },
    quota: {
      valid: true,
      status_code: 200,
      checked_at: hours(-0.5),
      raw_data: {
        data: {
          quota: {
            fast_lane: {
              window_seconds: 14400,
              consumed: 72,
              total: 100,
              resetAt: hours(0.8),
            },
            project_pool: {
              duration: "14 days",
              percentage: 57,
              left: 860,
              expires_at: days(9),
            },
          },
        },
      },
    },
  },
  {
    account_id: "acct_delta_max",
    email: "delta-max@example.test",
    file_path: "auths/delta-max@example.test.json",
    status: "active",
    plan_type: "max",
    has_refresh_token: true,
    total_requests: 120944,
    total_errors: 44,
    consecutive_failures: 0,
    last_used_at: hours(-0.03),
    last_refreshed_at: hours(-5),
    quota_exhausted: false,
    token_expire: days(31),
    usage: {
      total_completions: 3901,
      input_tokens: 12224000,
      output_tokens: 6402000,
      total_tokens: 18626000,
    },
    quota: {
      valid: true,
      status_code: 200,
      checked_at: hours(-0.03),
      raw_data: {
        tier: "max",
        usage_windows: {
          primary: {
            period: "5h",
            used_percent: 79,
            remaining: 21,
            reset_at: hours(0.6),
          },
          secondary: {
            period: "7d",
            used: 6330,
            limit: 10000,
            remaining: 3670,
            reset_at: days(2),
          },
          burst: {
            seconds: 3600,
            usage_percent: 18,
            remaining: 82,
            reset_at: hours(0.4),
          },
        },
      },
    },
  },
  {
    account_id: "acct_echo_disabled",
    email: "echo-disabled@example.test",
    file_path: "auths/echo-disabled@example.test.json.disabled",
    status: "disabled",
    plan_type: "free",
    has_refresh_token: false,
    disable_reason: "refresh token expired",
    total_requests: 920,
    total_errors: 121,
    consecutive_failures: 6,
    last_used_at: days(-2),
    last_refreshed_at: days(-6),
    quota_exhausted: false,
    token_expire: days(-1),
    usage: {
      total_completions: 22,
      input_tokens: 41800,
      output_tokens: 9300,
      total_tokens: 51100,
    },
    quota: {
      valid: false,
      status_code: 401,
      checked_at: days(-1),
    },
  },
  {
    account_id: "acct_foxtrot_exhausted",
    email: "foxtrot-exhausted@example.test",
    file_path: "auths/foxtrot-exhausted@example.test.json",
    status: "active",
    plan_type: "plus",
    has_refresh_token: true,
    total_requests: 23110,
    total_errors: 19,
    consecutive_failures: 0,
    last_used_at: hours(-0.2),
    last_refreshed_at: hours(-4),
    quota_exhausted: true,
    quota_resets_at: hours(4.5),
    token_expire: days(12),
    usage: {
      total_completions: 719,
      input_tokens: 2111000,
      output_tokens: 980000,
      total_tokens: 3091000,
    },
    quota: {
      valid: true,
      status_code: 200,
      checked_at: hours(-0.2),
      raw_data: {
        quota: {
          duration: "5 hours",
          used: 80,
          limit: 80,
          remaining: 0,
          reset_at: hours(4.5),
        },
      },
    },
  },
]

function readBody(req: IncomingMessage) {
  return new Promise<string>((resolve, reject) => {
    let body = ""
    req.setEncoding("utf8")
    req.on("data", (chunk) => {
      body += chunk
    })
    req.on("end", () => resolve(body))
    req.on("error", reject)
  })
}

function sendJson(res: ServerResponse, payload: unknown, statusCode = 200) {
  res.statusCode = statusCode
  res.setHeader("Content-Type", "application/json; charset=utf-8")
  res.end(JSON.stringify(payload))
}

function sendSse(res: ServerResponse, events: unknown[]) {
  res.statusCode = 200
  res.setHeader("Content-Type", "text/event-stream; charset=utf-8")
  res.setHeader("Cache-Control", "no-cache")
  for (const event of events) {
    res.write(`data: ${JSON.stringify(event)}\n\n`)
  }
  res.end()
}

function accountMatches(account: MockAccount, email?: string, filePath?: string) {
  const cleanEmail = email?.trim().toLowerCase()
  const cleanFilePath = filePath?.trim()
  return (
    Boolean(cleanEmail && account.email.toLowerCase() === cleanEmail) ||
    Boolean(
      cleanFilePath &&
        (account.file_path === cleanFilePath ||
          path.basename(account.file_path) === path.basename(cleanFilePath))
    )
  )
}

function findMockAccount(email?: string, filePath?: string) {
  return mockAccounts.find((account) => accountMatches(account, email, filePath))
}

function countIngestAccounts(body: string) {
  try {
    const parsed = JSON.parse(body)
    return Array.isArray(parsed) ? parsed.length : 1
  } catch {
    return body
      .split(/\r?\n/)
      .map((line) => line.trim())
      .filter(Boolean).length
  }
}

function makeMockAccount(input: Record<string, unknown>) {
  const email =
    typeof input.email === "string" && input.email.trim()
      ? input.email.trim()
      : `uploaded-${Date.now()}@example.test`
  const accountId =
    typeof input.account_id === "string" && input.account_id.trim()
      ? input.account_id.trim()
      : `acct_${email.replace(/[^a-z0-9]+/gi, "_")}`
  const nowIso = new Date().toISOString()

  return {
    account_id: accountId,
    email,
    file_path: `auths/${email}.json`,
    status:
      typeof input.status === "string" && input.status.trim()
        ? input.status.trim()
        : "active",
    plan_type:
      typeof input.plan_type === "string" && input.plan_type.trim()
        ? input.plan_type.trim()
        : "unknown",
    has_refresh_token: Boolean(input.refresh_token || input.rk),
    disable_reason:
      typeof input.disable_reason === "string" ? input.disable_reason : undefined,
    total_requests: 0,
    total_errors: 0,
    consecutive_failures: 0,
    last_used_at: nowIso,
    last_refreshed_at: nowIso,
    quota_exhausted: false,
    token_expire:
      typeof input.expired === "string" && input.expired.trim()
        ? input.expired.trim()
        : days(14),
    usage: {
      total_completions: 0,
      input_tokens: 0,
      output_tokens: 0,
      total_tokens: 0,
    },
    quota: {
      valid: true,
      status_code: 200,
      checked_at: nowIso,
      raw_data: {
        quota: {
          period: "5h",
          used_percent: 0,
          remaining: 80,
          reset_at: hours(5),
        },
      },
    },
  } satisfies MockAccount
}

function devApiMock() {
  return {
    name: "codex-proxy-dev-api-mock",
    apply: "serve" as const,
    configureServer(server: { middlewares: { use: (handler: Middleware) => void } }) {
      server.middlewares.use((req, res, next) => {
        if (!req.url) return next()
        const url = new URL(req.url, "http://localhost")

        if (req.method === "GET" && url.pathname === "/stats") {
          const query = url.searchParams.get("q")?.trim().toLowerCase() ?? ""
          const page = Number(url.searchParams.get("page") || "1")
          const pageSize = Number(url.searchParams.get("page_size") || "50")
          const filtered = query
            ? mockAccounts.filter((account) =>
                account.email.toLowerCase().includes(query)
              )
            : mockAccounts
          const start = Math.max(0, (page - 1) * pageSize)
          const accounts = filtered.slice(start, start + pageSize)
          const summary = {
            total: mockAccounts.length,
            active: mockAccounts.filter((account) => account.status === "active").length,
            cooldown: mockAccounts.filter((account) => account.status === "cooldown").length,
            disabled: mockAccounts.filter((account) => account.status === "disabled").length,
            rpm: 37,
            total_input_tokens: mockAccounts.reduce(
              (sum, account) => sum + account.usage.input_tokens,
              0
            ),
            total_output_tokens: mockAccounts.reduce(
              (sum, account) => sum + account.usage.output_tokens,
              0
            ),
          }
          return sendJson(res, {
            summary,
            accounts,
            pagination: {
              page,
              page_size: pageSize,
              total: mockAccounts.length,
              filtered_total: filtered.length,
              total_pages: Math.max(1, Math.ceil(filtered.length / pageSize)),
              returned: accounts.length,
              has_prev: page > 1,
              has_next: start + pageSize < filtered.length,
              query,
            },
          })
        }

        if (req.method === "POST" && url.pathname === "/check-quota") {
          return sendSse(res, [
            { type: "start", message: "mock quota check", current: 0, total: 6 },
            {
              type: "account",
              email: "alpha.plus@example.test",
              success: true,
              current: 1,
              total: 6,
              message: "5h / 7d windows updated",
            },
            {
              type: "done",
              success_count: 5,
              failed_count: 1,
              remaining: 5,
              duration: "420ms",
            },
          ])
        }

        if (req.method === "POST" && url.pathname === "/refresh") {
          return sendSse(res, [
            { type: "start", message: "mock token refresh", current: 0, total: 6 },
            {
              type: "account",
              email: "echo-disabled@example.test",
              success: false,
              current: 1,
              total: 6,
              message: "refresh token expired",
            },
            {
              type: "done",
              success_count: 5,
              failed_count: 1,
              duration: "510ms",
            },
          ])
        }

        if (req.method === "POST" && url.pathname === "/admin/accounts/ingest") {
          void readBody(req)
            .then((body) => {
              const trimmed = body.trim()
              if (!trimmed) {
                sendJson(res, { message: "empty ingest payload" }, 400)
                return
              }

              const accountCount = countIngestAccounts(trimmed)

              if (accountCount <= 0) {
                sendJson(res, { message: "no accounts detected" }, 400)
                return
              }

              const updated = accountCount > 1 ? 1 : 0
              sendJson(res, {
                added: Math.max(0, accountCount - updated),
                updated,
                failed: 0,
                pool_total: mockAccounts.length + Math.max(0, accountCount - updated),
              })
            })
            .catch(() => {
              sendJson(res, { message: "failed to read ingest payload" }, 400)
            })
          return
        }

        if (req.method === "POST" && url.pathname === "/admin/accounts") {
          void readBody(req)
            .then((body) => {
              const payload = JSON.parse(body || "{}") as Record<string, unknown>
              const account = makeMockAccount(payload)
              const existing = findMockAccount(account.email, account.file_path)
              if (existing) {
                Object.assign(existing, account)
                sendJson(res, {
                  added: 0,
                  updated: 1,
                  failed: 0,
                  pool_total: mockAccounts.length,
                })
                return
              }
              mockAccounts.unshift(account)
              sendJson(res, {
                added: 1,
                updated: 0,
                failed: 0,
                pool_total: mockAccounts.length,
              })
            })
            .catch(() => {
              sendJson(res, { message: "failed to create mock account" }, 400)
            })
          return
        }

        if (req.method === "PUT" && url.pathname === "/admin/accounts") {
          void readBody(req)
            .then((body) => {
              const payload = JSON.parse(body || "{}") as {
                email?: string
                file_path?: string
                account?: Record<string, unknown>
              }
              const account = findMockAccount(payload.email, payload.file_path)
              if (!account) {
                sendJson(res, { message: "mock account not found" }, 404)
                return
              }
              const patch = payload.account ?? {}
              if (typeof patch.email === "string") account.email = patch.email
              if (typeof patch.account_id === "string") {
                account.account_id = patch.account_id
              }
              if (typeof patch.plan_type === "string") {
                account.plan_type = patch.plan_type
              }
              if (typeof patch.status === "string") account.status = patch.status
              if (typeof patch.disable_reason === "string") {
                account.disable_reason = patch.disable_reason || undefined
              }
              if (typeof patch.expired === "string") account.token_expire = patch.expired
              if (patch.refresh_token || patch.rk) account.has_refresh_token = true
              sendJson(res, { object: "account", account })
            })
            .catch(() => {
              sendJson(res, { message: "failed to update mock account" }, 400)
            })
          return
        }

        if (req.method === "DELETE" && url.pathname === "/admin/accounts") {
          void readBody(req)
            .then((body) => {
              const payload = JSON.parse(body || "{}") as {
                email?: string
                file_path?: string
                hard?: boolean
              }
              const index = mockAccounts.findIndex((account) =>
                accountMatches(account, payload.email, payload.file_path)
              )
              if (index < 0) {
                sendJson(res, { message: "mock account not found" }, 404)
                return
              }
              const [account] = mockAccounts.splice(index, 1)
              sendJson(res, {
                object: "account_delete_result",
                email: account.email,
                file_path: account.file_path,
                hard: payload.hard ?? true,
              })
            })
            .catch(() => {
              sendJson(res, { message: "failed to delete mock account" }, 400)
            })
          return
        }

        if (req.method === "POST" && url.pathname === "/admin/accounts/probe") {
          void readBody(req)
            .then((body) => {
              const payload = JSON.parse(body || "{}") as {
                email?: string
                file_path?: string
              }
              const account = findMockAccount(payload.email, payload.file_path)
              if (!account) {
                sendJson(res, { message: "mock account not found" }, 404)
                return
              }
              const status =
                account.status === "disabled"
                  ? "invalid"
                  : account.status === "cooldown"
                    ? "rate_limited"
                    : "ok"
              const httpStatus =
                status === "invalid" ? 401 : status === "rate_limited" ? 429 : 200
              account.quota = {
                valid: status !== "invalid",
                status_code: httpStatus,
                checked_at: new Date().toISOString(),
                raw_data:
                  status === "invalid"
                    ? undefined
                    : {
                        usage_windows: {
                          primary: {
                            period: "5h",
                            used_percent: status === "rate_limited" ? 100 : 47,
                            remaining: status === "rate_limited" ? 0 : 53,
                            reset_at: hours(2),
                          },
                          secondary: {
                            period: "7d",
                            used: 280,
                            limit: 1000,
                            remaining: 720,
                            reset_at: days(4),
                          },
                        },
                      },
              }
              sendJson(res, {
                object: "account_probe_result",
                email: account.email,
                file_path: account.file_path,
                status,
                verdict: status === "ok" ? 1 : status === "rate_limited" ? 2 : -1,
                http_status: httpStatus,
                message:
                  status === "ok"
                    ? "mock account is available"
                    : status === "rate_limited"
                      ? "mock account is rate limited"
                      : "mock account is invalid",
                checked_at: account.quota.checked_at,
                quota: account.quota,
              })
            })
            .catch(() => {
              sendJson(res, { message: "failed to probe mock account" }, 400)
            })
          return
        }

        return next()
      })
    },
  }
}

// https://vite.dev/config/
export default defineConfig({
  base: "/",
  plugins: [devApiMock(), react(), tailwindcss()],
  build: {
    outDir: "../../../internal/static/assets",
    emptyOutDir: true,
  },
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
})
