import { Activity, Gauge, Loader2, PencilLine, Trash2 } from "lucide-react"

import { Badge } from "@workspace/ui/components/badge"
import { Button } from "@workspace/ui/components/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui/components/card"
import { Skeleton } from "@workspace/ui/components/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui/components/table"

import type { AccountProbeResult, AccountStats } from "@/lib/api"
import { accountKey } from "@/lib/account"
import { formatDate, formatDateTime, formatNumber } from "@/lib/format"
import { QuotaWindows, StatusBadge } from "@/components/account-shared"

function probeBadgeVariant(status: AccountProbeResult["status"]) {
  if (status === "ok") return "default"
  if (status === "rate_limited") return "secondary"
  if (status === "invalid") return "outline"
  return "destructive"
}

function probeBadgeLabel(status: AccountProbeResult["status"]) {
  switch (status) {
    case "ok":
      return "可用"
    case "rate_limited":
      return "限流"
    case "invalid":
      return "无效"
    case "transient_failed":
      return "暂态失败"
    default:
      return status || "未知"
  }
}

function LoadingRows() {
  return Array.from({ length: 6 }, (_, index) => (
    <TableRow key={index}>
      <TableCell colSpan={11}>
        <Skeleton className="h-8 w-full" />
      </TableCell>
    </TableRow>
  ))
}

function EmptyState() {
  return (
    <div className="flex min-h-40 flex-col items-center justify-center gap-2 text-center">
      <Activity className="size-8 text-muted-foreground" />
      <div className="font-medium">没有匹配账号</div>
      <div className="max-w-sm text-sm text-muted-foreground">
        调整搜索、状态筛选，或直接新增一个账号。
      </div>
    </div>
  )
}

function ActionButton({
  label,
  onClick,
  variant,
  icon,
  disabled,
}: {
  label: string
  onClick: () => void
  variant?: "default" | "secondary" | "outline" | "destructive" | "ghost"
  icon: typeof PencilLine
  disabled?: boolean
}) {
  const Icon = icon
  return (
    <Button size="sm" variant={variant} onClick={onClick} disabled={disabled}>
      <Icon data-icon="inline-start" />
      {label}
    </Button>
  )
}

function QuotaCheckSummary({
  result,
}: {
  result?: AccountProbeResult
}) {
  if (!result) return null
  return (
    <div className="flex flex-col gap-1 text-xs">
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant={probeBadgeVariant(result.status)}>
          {probeBadgeLabel(result.status)}
        </Badge>
        <span className="text-muted-foreground">
          {result.http_status > 0 ? `HTTP ${result.http_status}` : "无 HTTP 状态"}
        </span>
        <span className="text-muted-foreground">
          {formatDateTime(result.checked_at)}
        </span>
      </div>
      {result.message ? (
        <div className="line-clamp-2 text-muted-foreground">{result.message}</div>
      ) : null}
    </div>
  )
}

function AccountRuntimeDetails({ account }: { account: AccountStats }) {
  const quotaStatus = account.quota
    ? account.quota.valid
      ? `有效${account.quota.status_code > 0 ? ` · HTTP ${account.quota.status_code}` : ""}`
      : `无效${account.quota.status_code > 0 ? ` · HTTP ${account.quota.status_code}` : ""}`
    : "未检查"

  return (
    <div className="grid grid-cols-2 gap-x-3 gap-y-2 rounded-lg border bg-background p-2 text-xs">
      <div>
        <div className="text-muted-foreground">Token 过期</div>
        <div className="mt-1 font-medium">{formatDateTime(account.token_expire)}</div>
      </div>
      <div>
        <div className="text-muted-foreground">最近刷新</div>
        <div className="mt-1 font-medium">{formatDateTime(account.last_refreshed_at)}</div>
      </div>
      <div>
        <div className="text-muted-foreground">冷却截止</div>
        <div className="mt-1 font-medium">{formatDateTime(account.cooldown_until)}</div>
      </div>
      <div>
        <div className="text-muted-foreground">连续失败</div>
        <div className="mt-1 font-mono font-medium">
          {formatNumber(account.consecutive_failures)}
        </div>
      </div>
      <div>
        <div className="text-muted-foreground">额度状态</div>
        <div className="mt-1 font-medium">{quotaStatus}</div>
      </div>
      <div>
        <div className="text-muted-foreground">额度检查</div>
        <div className="mt-1 font-medium">{formatDateTime(account.quota?.checked_at)}</div>
      </div>
    </div>
  )
}

function UsageSummary({ account }: { account: AccountStats }) {
  return (
    <div className="grid grid-cols-2 gap-2 text-xs">
      <div className="rounded-lg bg-muted p-2">
        <div className="text-muted-foreground">完成次数</div>
        <div className="mt-1 font-mono font-medium">
          {formatNumber(account.usage.total_completions)}
        </div>
      </div>
      <div className="rounded-lg bg-muted p-2">
        <div className="text-muted-foreground">输入 token</div>
        <div className="mt-1 font-mono font-medium">
          {formatNumber(account.usage.input_tokens)}
        </div>
      </div>
      <div className="rounded-lg bg-muted p-2">
        <div className="text-muted-foreground">输出 token</div>
        <div className="mt-1 font-mono font-medium">
          {formatNumber(account.usage.output_tokens)}
        </div>
      </div>
      <div className="rounded-lg bg-muted p-2">
        <div className="text-muted-foreground">总 token</div>
        <div className="mt-1 font-mono font-medium">
          {formatNumber(account.usage.total_tokens)}
        </div>
      </div>
      <div className="rounded-lg bg-muted p-2">
        <div className="text-muted-foreground">缓存读取</div>
        <div className="mt-1 font-mono font-medium">
          {formatNumber(account.usage.cache_read_tokens)}
        </div>
      </div>
      <div className="rounded-lg bg-muted p-2">
        <div className="text-muted-foreground">缓存写入</div>
        <div className="mt-1 font-mono font-medium">
          {formatNumber(account.usage.cache_write_tokens)}
        </div>
      </div>
      <div className="rounded-lg bg-muted p-2">
        <div className="text-muted-foreground">推理 token</div>
        <div className="mt-1 font-mono font-medium">
          {formatNumber(account.usage.reasoning_tokens)}
        </div>
      </div>
    </div>
  )
}

function ProbeActions({
  account,
  keyId,
  probingKey,
  deletingKey,
  onEdit,
  onDelete,
  onProbe,
}: {
  account: AccountStats
  keyId: string
  probingKey: string | null
  deletingKey: string | null
  onEdit: (account: AccountStats) => void
  onDelete: (account: AccountStats) => void
  onProbe: (account: AccountStats) => void
}) {
  const probing = probingKey === keyId
  const deleting = deletingKey === keyId

  return (
    <div className="flex flex-wrap gap-2">
      <ActionButton
        label={probing ? "查询中" : "查额度"}
        onClick={() => onProbe(account)}
        variant="outline"
        icon={probing ? Loader2 : Gauge}
        disabled={probing || deleting}
      />
      <ActionButton
        label="编辑"
        onClick={() => onEdit(account)}
        variant="secondary"
        icon={PencilLine}
        disabled={probing || deleting}
      />
      <ActionButton
        label={deleting ? "删除中" : "删除"}
        onClick={() => onDelete(account)}
        variant="destructive"
        icon={deleting ? Loader2 : Trash2}
        disabled={probing || deleting}
      />
    </div>
  )
}

export function AccountTable({
  accounts,
  loading,
  probingKey,
  deletingKey,
  probeResults,
  onEdit,
  onDelete,
  onProbe,
}: {
  accounts: AccountStats[]
  loading: boolean
  probingKey: string | null
  deletingKey: string | null
  probeResults: Record<string, AccountProbeResult>
  onEdit: (account: AccountStats) => void
  onDelete: (account: AccountStats) => void
  onProbe: (account: AccountStats) => void
}) {
  if (loading) {
    return (
      <div className="flex flex-col gap-3">
        <div className="flex flex-col gap-3 lg:hidden">
          {Array.from({ length: 4 }, (_, index) => (
            <Skeleton key={index} className="h-36 w-full rounded-xl" />
          ))}
        </div>
        <div className="hidden overflow-x-auto lg:block">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>账号</TableHead>
                <TableHead>套餐</TableHead>
                <TableHead>状态</TableHead>
                <TableHead>额度窗口</TableHead>
                <TableHead>请求</TableHead>
                <TableHead>错误</TableHead>
                <TableHead>最后使用</TableHead>
                <TableHead>额度检查</TableHead>
                <TableHead>运行信息</TableHead>
                <TableHead>Token 用量</TableHead>
                <TableHead>操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>{LoadingRows()}</TableBody>
          </Table>
        </div>
      </div>
    )
  }

  if (accounts.length === 0) {
    return (
      <div className="flex flex-col gap-3">
        <div className="lg:hidden">
          <EmptyState />
        </div>
        <Card className="hidden lg:block">
          <CardHeader>
            <CardTitle>账号明细</CardTitle>
            <CardDescription>当前筛选下没有结果。</CardDescription>
          </CardHeader>
          <CardContent>
            <EmptyState />
          </CardContent>
        </Card>
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-4 lg:hidden">
        {accounts.map((account) => {
          const keyId = accountKey(account)
          const probe = probeResults[keyId]
          const displayedQuota = probe?.quota ?? account.quota
          return (
            <article
              key={keyId}
              className="flex flex-col gap-3 rounded-xl border bg-card p-3 shadow-sm"
            >
              <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                  <div className="truncate font-medium">{account.email || "未知账号"}</div>
                  <div className="truncate text-xs text-muted-foreground">
                    {account.account_id ? `ID ${account.account_id}` : "未设置 ID"}
                  </div>
                  <div className="truncate text-xs text-muted-foreground">
                    {account.file_path || "未设置文件路径"}
                  </div>
                </div>
                <div className="flex shrink-0 flex-col items-end gap-1">
                  <Badge variant="secondary">{account.plan_type || "unknown"}</Badge>
                  <StatusBadge status={account.status} />
                </div>
              </div>

              {account.disable_reason ? (
                <div className="text-xs text-muted-foreground">
                  {account.disable_reason}
                </div>
              ) : null}

              <QuotaWindows account={account} quota={displayedQuota} />
              <QuotaCheckSummary result={probe} />
              <AccountRuntimeDetails account={account} />

              <div className="grid grid-cols-4 gap-2 text-xs">
                <div className="rounded-lg bg-muted p-2">
                  <div className="text-muted-foreground">请求</div>
                  <div className="mt-1 font-mono font-medium">
                    {formatNumber(account.total_requests)}
                  </div>
                </div>
                <div className="rounded-lg bg-muted p-2">
                  <div className="text-muted-foreground">错误</div>
                  <div className="mt-1 font-mono font-medium">
                    {formatNumber(account.total_errors)}
                  </div>
                </div>
                <div className="rounded-lg bg-muted p-2">
                  <div className="text-muted-foreground">检查</div>
                  <div className="mt-1 text-[0.7rem] leading-snug">
                    {formatDate(displayedQuota?.checked_at)}
                  </div>
                </div>
                <div className="rounded-lg bg-muted p-2">
                  <div className="text-muted-foreground">连续失败</div>
                  <div className="mt-1 font-mono font-medium">
                    {formatNumber(account.consecutive_failures)}
                  </div>
                </div>
              </div>
              <UsageSummary account={account} />

              <ProbeActions
                account={account}
                keyId={keyId}
                probingKey={probingKey}
                deletingKey={deletingKey}
                onEdit={onEdit}
                onDelete={onDelete}
                onProbe={onProbe}
              />
            </article>
          )
        })}
      </div>

      <div className="hidden overflow-x-auto lg:block">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>账号</TableHead>
              <TableHead>套餐</TableHead>
              <TableHead>状态</TableHead>
              <TableHead>额度窗口</TableHead>
              <TableHead>请求</TableHead>
              <TableHead>错误</TableHead>
              <TableHead>最后使用</TableHead>
              <TableHead>额度检查</TableHead>
              <TableHead>Token 用量</TableHead>
              <TableHead>操作</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {accounts.map((account) => {
              const keyId = accountKey(account)
              const probe = probeResults[keyId]
              const displayedQuota = probe?.quota ?? account.quota
              return (
                <TableRow key={keyId}>
                  <TableCell>
                    <div className="flex min-w-52 flex-col gap-1">
                      <span className="truncate font-medium">
                        {account.email || "未知账号"}
                      </span>
                      <span className="truncate text-xs text-muted-foreground">
                        {account.account_id ? `ID ${account.account_id}` : "未设置 ID"}
                      </span>
                      <span className="truncate text-xs text-muted-foreground">
                        {account.file_path || "未设置文件路径"}
                      </span>
                    </div>
                  </TableCell>
                  <TableCell>
                    <Badge variant="secondary">{account.plan_type || "unknown"}</Badge>
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-col gap-1">
                      <StatusBadge status={account.status} />
                      {account.disable_reason ? (
                        <span className="text-xs text-muted-foreground">
                          {account.disable_reason}
                        </span>
                      ) : null}
                    </div>
                  </TableCell>
                  <TableCell className="whitespace-normal">
                    <div className="flex flex-col gap-2">
                      <QuotaWindows account={account} quota={displayedQuota} />
                      <QuotaCheckSummary result={probe} />
                    </div>
                  </TableCell>
                  <TableCell className="font-mono">
                    {formatNumber(account.total_requests)}
                  </TableCell>
                  <TableCell className="font-mono">
                    {formatNumber(account.total_errors)}
                  </TableCell>
                  <TableCell>{formatDate(account.last_used_at)}</TableCell>
                  <TableCell>
                    <div className="flex min-w-36 flex-col gap-1 text-xs">
                      <span>刷新 {formatDate(account.last_refreshed_at)}</span>
                      <span>过期 {formatDate(account.token_expire)}</span>
                      <span>失败 {formatNumber(account.consecutive_failures)} 次</span>
                      <span>冷却 {formatDate(account.cooldown_until)}</span>
                    </div>
                  </TableCell>
                  <TableCell>
                    <div className="flex min-w-32 flex-col gap-1 font-mono text-xs">
                      <span>完成 {formatNumber(account.usage.total_completions)}</span>
                      <span>输入 {formatNumber(account.usage.input_tokens)}</span>
                      <span>输出 {formatNumber(account.usage.output_tokens)}</span>
                      <span>总计 {formatNumber(account.usage.total_tokens)}</span>
                      <span>缓存读 {formatNumber(account.usage.cache_read_tokens)}</span>
                      <span>缓存写 {formatNumber(account.usage.cache_write_tokens)}</span>
                      <span>推理 {formatNumber(account.usage.reasoning_tokens)}</span>
                    </div>
                  </TableCell>
                  <TableCell>
                    <ProbeActions
                      account={account}
                      keyId={keyId}
                      probingKey={probingKey}
                      deletingKey={deletingKey}
                      onEdit={onEdit}
                      onDelete={onDelete}
                      onProbe={onProbe}
                    />
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      </div>
    </div>
  )
}
