import {
  type FormEvent,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react"
import {
  Activity,
  AlertCircle,
  KeyRound,
  Plus,
  RefreshCw,
  Search,
  Settings,
  ShieldCheck,
  Upload,
} from "lucide-react"

import { Alert, AlertDescription, AlertTitle } from "@workspace/ui/components/alert"
import { Badge } from "@workspace/ui/components/badge"
import { Button } from "@workspace/ui/components/button"
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui/components/card"
import { Field, FieldGroup, FieldLabel } from "@workspace/ui/components/field"
import { Input } from "@workspace/ui/components/input"
import { Progress } from "@workspace/ui/components/progress"
import { Separator } from "@workspace/ui/components/separator"
import { Textarea } from "@workspace/ui/components/textarea"
import { Tabs, TabsList, TabsTrigger } from "@workspace/ui/components/tabs"
import { TooltipProvider } from "@workspace/ui/components/tooltip"

import { AccountEditor } from "@/components/account-editor"
import { AccountTable } from "@/components/account-table"
import { DashboardScreen } from "@/components/dashboard-screen"
import {
  type AccountProbeResult,
  type AccountStats,
  type Credentials,
  type IngestResult,
  type ProgressEvent,
  type StatsResponse,
  deleteAccount,
  fetchStats,
  ingestAccounts,
  isAuthError,
  loadCredentials,
  probeAccount,
  runProgressStream,
  saveCredentials,
} from "@/lib/api"
import { accountKey } from "@/lib/account"
import { formatNumber } from "@/lib/format"

type AccountTab = "all" | "active" | "cooldown" | "disabled" | "exhausted"
type RunningAction = "quota" | "refresh" | null
type ImportMode = "single" | "bulk"

const pageSizes = [25, 50, 100, 200]

function actionLabel(action: RunningAction) {
  if (action === "quota") return "额度检查"
  if (action === "refresh") return "Token 刷新"
  return "后台任务"
}

function ProgressPanel({
  action,
  events,
}: {
  action: RunningAction
  events: ProgressEvent[]
}) {
  if (!action) return null
  const latest = events[0]
  const measured = events.find(
    (event) =>
      typeof event.current === "number" && typeof event.total === "number"
  )
  const done = latest?.type === "done"
  const total = latest?.total ?? measured?.total ?? 0
  const current =
    latest?.current ?? (done && total > 0 ? total : measured?.current ?? 0)
  const percent =
    done || latest?.success ? 100 : total > 0 ? Math.round((current / total) * 100) : 0
  const progressText =
    total > 0 ? `${Math.min(current, total)} / ${total}` : done ? "已完成" : "等待事件"

  return (
    <Alert>
      <Activity />
      <AlertTitle>{actionLabel(action)}进行中</AlertTitle>
      <AlertDescription>
        <div className="mt-2 flex flex-col gap-3">
          <Progress value={percent} />
          <div className="flex flex-wrap items-center gap-3 text-xs">
            <span>{progressText}</span>
            {latest?.email ? <span>{latest.email}</span> : null}
            {latest?.message ? <span>{latest.message}</span> : null}
          </div>
        </div>
      </AlertDescription>
    </Alert>
  )
}

function SettingsPanel({
  credentials,
  onSave,
}: {
  credentials: Credentials
  onSave: (credentials: Credentials) => void
}) {
  const [apiUrl, setApiUrl] = useState(credentials.apiUrl)
  const [token, setToken] = useState(credentials.token)

  return (
    <Card>
      <CardHeader>
        <CardTitle>连接设置</CardTitle>
        <CardDescription>
          默认使用当前同源服务。开启 API Key 后填写 Bearer Token。
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form
          className="grid gap-3 md:grid-cols-[1fr_1fr_auto]"
          onSubmit={(event) => {
            event.preventDefault()
            const next = { apiUrl: apiUrl.trim(), token: token.trim() }
            saveCredentials(next)
            onSave(next)
          }}
        >
          <FieldGroup className="contents">
            <Field>
              <FieldLabel htmlFor="stats-api">Stats API</FieldLabel>
              <Input
                id="stats-api"
                value={apiUrl}
                onChange={(event) => setApiUrl(event.target.value)}
                placeholder="http://127.0.0.1:8080/stats"
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="bearer-token">Bearer Token</FieldLabel>
              <Input
                id="bearer-token"
                value={token}
                onChange={(event) => setToken(event.target.value)}
                placeholder="留空表示无需鉴权"
                type="password"
              />
            </Field>
          </FieldGroup>
          <div className="flex items-end">
            <Button type="submit" className="w-full md:w-auto">
              <KeyRound data-icon="inline-start" />
              保存连接
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  )
}

function ImportAccountsPanel({
  credentials,
  onClose,
  onImported,
}: {
  credentials: Credentials
  onClose: () => void
  onImported: () => void
}) {
  const [mode, setMode] = useState<ImportMode>("single")
  const [email, setEmail] = useState("")
  const [refreshToken, setRefreshToken] = useState("")
  const [rk, setRk] = useState("")
  const [bulkText, setBulkText] = useState("")
  const [submitting, setSubmitting] = useState(false)
  const [result, setResult] = useState<IngestResult | null>(null)
  const [error, setError] = useState("")

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setSubmitting(true)
    setError("")
    setResult(null)

    try {
      let payload = ""
      let contentType: "application/json" | "application/x-ndjson" = "application/json"

      if (mode === "single") {
        if (!refreshToken.trim() && !rk.trim()) {
          throw new Error("请填写 refresh token 或 rk")
        }
        payload = JSON.stringify({
          email: email.trim() || undefined,
          refresh_token: refreshToken.trim() || undefined,
          rk: rk.trim() || undefined,
          type: "codex",
        })
      } else {
        const trimmed = bulkText.trim()
        if (!trimmed) throw new Error("请粘贴 JSON 数组或 NDJSON")
        try {
          JSON.parse(trimmed)
          contentType = "application/json"
        } catch {
          contentType = "application/x-ndjson"
        }
        payload = trimmed
      }

      const next = await ingestAccounts(credentials, payload, contentType)
      setResult(next)
      onImported()
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "导入失败")
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Card>
      <CardHeader className="gap-3">
        <div>
          <CardTitle>导入账号</CardTitle>
          <CardDescription>
            支持单个账号补录，也支持 JSON 数组或 NDJSON 批量导入。
          </CardDescription>
        </div>
        <CardAction>
          <Button variant="ghost" size="sm" onClick={onClose}>
            关闭
          </Button>
        </CardAction>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <Tabs
          value={mode}
          onValueChange={(value) => {
            setMode(value as ImportMode)
            setResult(null)
            setError("")
          }}
        >
          <TabsList>
            <TabsTrigger value="single">单个账号</TabsTrigger>
            <TabsTrigger value="bulk">批量导入</TabsTrigger>
          </TabsList>
        </Tabs>

        <form className="flex flex-col gap-4" onSubmit={handleSubmit}>
          {mode === "single" ? (
            <FieldGroup className="grid gap-4 md:grid-cols-3">
              <Field>
                <FieldLabel htmlFor="import-email">邮箱</FieldLabel>
                <Input
                  id="import-email"
                  value={email}
                  onChange={(event) => setEmail(event.target.value)}
                  placeholder="alpha.plus@example.test"
                />
              </Field>
              <Field>
                <FieldLabel htmlFor="import-refresh-token">Refresh token</FieldLabel>
                <Input
                  id="import-refresh-token"
                  value={refreshToken}
                  onChange={(event) => setRefreshToken(event.target.value)}
                  placeholder="可直接粘贴 refresh token"
                />
              </Field>
              <Field>
                <FieldLabel htmlFor="import-rk">RK</FieldLabel>
                <Input
                  id="import-rk"
                  value={rk}
                  onChange={(event) => setRk(event.target.value)}
                  placeholder="或 rk"
                />
              </Field>
            </FieldGroup>
          ) : (
            <FieldGroup>
              <Field>
                <FieldLabel htmlFor="bulk-text">批量内容</FieldLabel>
                <Textarea
                  id="bulk-text"
                  value={bulkText}
                  onChange={(event) => setBulkText(event.target.value)}
                  className="min-h-56 font-mono text-sm"
                  placeholder={`[
  {
    "email": "alpha.plus@example.test",
    "rk": "..."
  }
]\n\n或每行一个 JSON 对象`}
                />
              </Field>
            </FieldGroup>
          )}

          <div className="flex flex-wrap items-center gap-2">
            <Button type="submit" disabled={submitting}>
              <Upload data-icon="inline-start" />
              {submitting ? "导入中" : "提交导入"}
            </Button>
            <Button
              type="button"
              variant="outline"
              onClick={() => {
                setEmail("")
                setRefreshToken("")
                setRk("")
                setBulkText("")
                setResult(null)
                setError("")
              }}
              disabled={submitting}
            >
              清空
            </Button>
          </div>
        </form>

        {error ? (
          <Alert variant="destructive">
            <AlertCircle />
            <AlertTitle>导入失败</AlertTitle>
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}

        {result ? (
          <div className="grid grid-cols-2 gap-2 rounded-lg bg-muted p-3 text-sm md:grid-cols-4">
            <div>
              <div className="text-muted-foreground">新增</div>
              <div className="font-mono text-lg font-semibold">{result.added}</div>
            </div>
            <div>
              <div className="text-muted-foreground">更新</div>
              <div className="font-mono text-lg font-semibold">{result.updated}</div>
            </div>
            <div>
              <div className="text-muted-foreground">失败</div>
              <div className="font-mono text-lg font-semibold">{result.failed}</div>
            </div>
            <div>
              <div className="text-muted-foreground">池总数</div>
              <div className="font-mono text-lg font-semibold">
                {result.pool_total}
              </div>
            </div>
            {result.errors?.length ? (
              <div className="col-span-2 text-xs text-muted-foreground md:col-span-4">
                {result.errors.slice(0, 5).join("；")}
              </div>
            ) : null}
          </div>
        ) : null}
      </CardContent>
    </Card>
  )
}

export function App() {
  const [credentials, setCredentials] = useState(loadCredentials)
  const [stats, setStats] = useState<StatsResponse | null>(null)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(50)
  const [query, setQuery] = useState("")
  const [tab, setTab] = useState<AccountTab>("all")
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState("")
  const [showSettings, setShowSettings] = useState(false)
  const [showImport, setShowImport] = useState(false)
  const [editingAccount, setEditingAccount] = useState<AccountStats | null>(null)
  const [showEditor, setShowEditor] = useState(false)
  const [runningAction, setRunningAction] = useState<RunningAction>(null)
  const [progressEvents, setProgressEvents] = useState<ProgressEvent[]>([])
  const [probeResults, setProbeResults] = useState<Record<string, AccountProbeResult>>({})
  const [probingKey, setProbingKey] = useState<string | null>(null)
  const [deletingKey, setDeletingKey] = useState<string | null>(null)
  const loadRequestId = useRef(0)

  const loadStats = useCallback(
    async (signal?: AbortSignal) => {
      const requestId = loadRequestId.current + 1
      loadRequestId.current = requestId
      setLoading(true)
      setError("")
      try {
        const next = await fetchStats(
          credentials,
          { page, pageSize, query, includeQuota: true },
          signal
        )
        if (signal?.aborted || requestId !== loadRequestId.current) return
        setStats(next)
        const nextTotalPages = Math.max(1, next.pagination?.total_pages ?? 1)
        const nextPage = next.pagination?.page ?? page
        if (nextPage > nextTotalPages) setPage(nextTotalPages)
      } catch (caught) {
        if (caught instanceof DOMException && caught.name === "AbortError") return
        if (signal?.aborted || requestId !== loadRequestId.current) return
        const message = caught instanceof Error ? caught.message : "无法加载统计数据"
        setError(message)
        if (isAuthError(caught)) setShowSettings(true)
      } finally {
        if (!signal?.aborted && requestId === loadRequestId.current) {
          setLoading(false)
        }
      }
    },
    [credentials, page, pageSize, query]
  )

  useEffect(() => {
    const controller = new AbortController()
    void loadStats(controller.signal)
    return () => controller.abort()
  }, [loadStats])

  const accounts = useMemo(() => stats?.accounts ?? [], [stats?.accounts])
  const filteredAccounts = useMemo(() => {
    if (tab === "all") return accounts
    if (tab === "exhausted") {
      return accounts.filter((account) => account.quota_exhausted)
    }
    return accounts.filter((account) => account.status === tab)
  }, [accounts, tab])

  const pagination = stats?.pagination
  const totalPages = pagination?.total_pages ?? 1

  async function runAction(action: Exclude<RunningAction, null>) {
    setRunningAction(action)
    setProgressEvents([])
    setError("")
    try {
      await runProgressStream(
        credentials,
        action === "quota" ? "/check-quota" : "/refresh",
        (event) => setProgressEvents((events) => [event, ...events].slice(0, 8))
      )
      await loadStats()
    } catch (caught) {
      const message = caught instanceof Error ? caught.message : `${actionLabel(action)}失败`
      setError(message)
      if (isAuthError(caught)) setShowSettings(true)
    } finally {
      setRunningAction(null)
    }
  }

  async function handleProbe(account: AccountStats) {
    const key = accountKey(account)
    setProbingKey(key)
    setError("")
    try {
      const result = await probeAccount(credentials, account)
      setProbeResults((current) => ({ ...current, [key]: result }))
      await loadStats()
    } catch (caught) {
      const message = caught instanceof Error ? caught.message : "额度查询失败"
      setError(message)
      if (isAuthError(caught)) setShowSettings(true)
    } finally {
      setProbingKey(null)
    }
  }

  async function handleDelete(account: AccountStats) {
    const key = accountKey(account)
    const name = account.email || account.account_id || account.file_path || "该账号"
    if (!window.confirm(`确认删除 ${name}？`)) return
    setDeletingKey(key)
    setError("")
    try {
      await deleteAccount(credentials, account, true)
      setProbeResults((current) => {
        const next = { ...current }
        delete next[key]
        return next
      })
      await loadStats()
    } catch (caught) {
      const message = caught instanceof Error ? caught.message : "删除账号失败"
      setError(message)
      if (isAuthError(caught)) setShowSettings(true)
    } finally {
      setDeletingKey(null)
    }
  }

  return (
    <TooltipProvider>
      <main className="min-h-svh bg-background">
        <div className="mx-auto flex w-full max-w-7xl flex-col gap-4 px-3 py-4 sm:px-4 md:gap-5 md:px-6 md:py-5 lg:px-8">
          <header className="flex flex-col gap-4 rounded-xl border bg-card p-3 ring-1 ring-foreground/5 sm:p-4 xl:flex-row xl:items-center xl:justify-between">
            <div className="flex min-w-0 flex-col gap-1">
              <div className="flex flex-wrap items-center gap-2">
                <Badge variant="outline">Codex Proxy</Badge>
                <Badge variant="secondary">管理控制台</Badge>
              </div>
              <h1 className="font-heading text-2xl font-semibold tracking-normal md:text-3xl">
                账号池数据大屏
              </h1>
              <p className="max-w-2xl text-sm text-muted-foreground">
                集中查看账号状态、额度限制、token 消耗，并完成账号增删改与单账号额度查询。
              </p>
            </div>
            <div className="grid grid-cols-2 gap-2 sm:flex sm:flex-wrap sm:items-center xl:justify-end">
              <Button
                className="w-full sm:w-auto"
                variant="outline"
                onClick={() => void loadStats()}
                disabled={loading || Boolean(runningAction)}
              >
                <RefreshCw data-icon="inline-start" />
                刷新数据
              </Button>
              <Button
                className="w-full sm:w-auto"
                onClick={() => void runAction("quota")}
                disabled={loading || Boolean(runningAction)}
              >
                <ShieldCheck data-icon="inline-start" />
                检查额度
              </Button>
              <Button
                className="w-full sm:w-auto"
                variant="secondary"
                onClick={() => void runAction("refresh")}
                disabled={loading || Boolean(runningAction)}
              >
                <KeyRound data-icon="inline-start" />
                刷新 Token
              </Button>
              <Button
                className="w-full sm:w-auto"
                variant="outline"
                onClick={() => setShowImport((value) => !value)}
              >
                <Upload data-icon="inline-start" />
                导入账号
              </Button>
              <Button
                className="w-full sm:w-auto"
                variant="outline"
                onClick={() => {
                  setEditingAccount(null)
                  setShowEditor((value) => !value)
                }}
              >
                <Plus data-icon="inline-start" />
                添加账号
              </Button>
              <Button
                variant="ghost"
                className="col-span-2 w-full sm:size-8 sm:w-auto"
                aria-label="连接设置"
                onClick={() => setShowSettings((value) => !value)}
              >
                <Settings data-icon="inline-start" />
                <span className="sm:hidden">连接设置</span>
              </Button>
            </div>
          </header>

          {showSettings ? (
            <SettingsPanel
              credentials={credentials}
              onSave={(next) => {
                setCredentials(next)
                setShowSettings(false)
                setPage(1)
              }}
            />
          ) : null}

          {showImport ? (
            <ImportAccountsPanel
              credentials={credentials}
              onClose={() => setShowImport(false)}
              onImported={() => void loadStats()}
            />
          ) : null}

          {showEditor ? (
            <AccountEditor
              credentials={credentials}
              account={editingAccount}
              onClose={() => {
                setShowEditor(false)
                setEditingAccount(null)
              }}
              onSaved={() => void loadStats()}
            />
          ) : null}

          {error ? (
            <Alert variant="destructive">
              <AlertCircle />
              <AlertTitle>请求失败</AlertTitle>
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          ) : null}

          <ProgressPanel action={runningAction} events={progressEvents} />
          <DashboardScreen stats={stats} />

          <Card>
            <CardHeader className="gap-3">
              <div>
                <CardTitle>账号明细</CardTitle>
                <CardDescription>
                  额度列按原始 raw_data 递归识别窗口，不假定所有账号都是 5h 或 7d。
                </CardDescription>
              </div>
              <CardAction className="hidden md:block">
                <Badge variant="outline">
                  {pagination
                    ? `${formatNumber(pagination.filtered_total)} 条`
                    : `${formatNumber(accounts.length)} 条`}
                </Badge>
              </CardAction>
            </CardHeader>
            <CardContent className="flex flex-col gap-4">
              <div className="flex flex-col gap-3 xl:flex-row xl:items-center xl:justify-between">
                <div className="-mx-1 overflow-x-auto px-1">
                  <Tabs
                    value={tab}
                    onValueChange={(value) => setTab(value as AccountTab)}
                  >
                    <TabsList className="min-w-max">
                      <TabsTrigger value="all">全部</TabsTrigger>
                      <TabsTrigger value="active">可用</TabsTrigger>
                      <TabsTrigger value="cooldown">冷却</TabsTrigger>
                      <TabsTrigger value="disabled">禁用</TabsTrigger>
                      <TabsTrigger value="exhausted">额度用尽</TabsTrigger>
                    </TabsList>
                  </Tabs>
                </div>

                <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
                  <div className="relative w-full sm:min-w-64">
                    <Search className="pointer-events-none absolute left-2.5 top-2 text-muted-foreground" />
                    <Input
                      className="pl-8"
                      value={query}
                      onChange={(event) => {
                        setQuery(event.target.value)
                        setPage(1)
                      }}
                      placeholder="搜索邮箱"
                    />
                  </div>
                  <select
                    className="h-8 rounded-lg border border-input bg-background px-2 text-sm"
                    value={pageSize}
                    onChange={(event) => {
                      setPageSize(Number(event.target.value))
                      setPage(1)
                    }}
                  >
                    {pageSizes.map((size) => (
                      <option key={size} value={size}>
                        {size} / 页
                      </option>
                    ))}
                  </select>
                </div>
              </div>

              <Separator />

              <AccountTable
                accounts={filteredAccounts}
                loading={loading}
                probingKey={probingKey}
                deletingKey={deletingKey}
                probeResults={probeResults}
                onEdit={(account) => {
                  setEditingAccount(account)
                  setShowEditor(true)
                }}
                onDelete={(account) => void handleDelete(account)}
                onProbe={(account) => void handleProbe(account)}
              />

              <div className="flex flex-col gap-2 text-sm text-muted-foreground sm:flex-row sm:items-center sm:justify-between">
                <span>
                  第 {formatNumber(pagination?.page ?? page)} /{" "}
                  {formatNumber(totalPages)} 页
                </span>
                <div className="flex items-center gap-2">
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={page <= 1 || loading}
                    onClick={() => setPage((value) => Math.max(1, value - 1))}
                  >
                    上一页
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={page >= totalPages || loading}
                    onClick={() => setPage((value) => value + 1)}
                  >
                    下一页
                  </Button>
                </div>
              </div>
            </CardContent>
          </Card>
        </div>
      </main>
    </TooltipProvider>
  )
}
