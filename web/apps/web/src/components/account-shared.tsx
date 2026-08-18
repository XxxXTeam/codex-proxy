import { Badge } from "@workspace/ui/components/badge"
import { Progress } from "@workspace/ui/components/progress"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@workspace/ui/components/tooltip"

import type { AccountStats } from "@/lib/api"
import { statusLabel, statusVariant } from "@/lib/account"
import { formatDate, formatDateTime, formatNumber, formatPercent } from "@/lib/format"
import { parseQuotaWindows, type QuotaWindow } from "@/lib/quota"

export function StatusBadge({ status }: { status: string }) {
  return (
    <Badge variant={statusVariant(status)} className="capitalize">
      {statusLabel(status)}
    </Badge>
  )
}

function quotaDetail(window: QuotaWindow) {
  const parts: string[] = []
  if (window.used !== undefined && window.limit !== undefined) {
    parts.push(`已用 ${formatNumber(window.used)} / ${formatNumber(window.limit)}`)
  } else if (window.limit !== undefined) {
    parts.push(`上限 ${formatNumber(window.limit)}`)
  } else if (window.used !== undefined) {
    parts.push(`已用 ${formatNumber(window.used)}`)
  }

  if (window.remaining !== undefined) {
    parts.push(`剩余 ${formatNumber(window.remaining)}`)
  }
  if (parts.length === 0 && window.usedPercent !== undefined) {
    parts.push(`已使用 ${formatPercent(window.usedPercent)}`)
  }

  return parts.length > 0 ? parts.join("，") : "上游未返回绝对额度"
}

export function QuotaWindows({
  account,
  quota = account.quota,
}: {
  account: AccountStats
  quota?: AccountStats["quota"]
}) {
  const windows = parseQuotaWindows(quota?.raw_data)

  if (!quota && account.quota_exhausted) {
    return (
      <Badge variant="destructive">
        已用尽 {formatDate(account.quota_resets_at)}
      </Badge>
    )
  }

  if (!quota) {
    return <span className="text-muted-foreground">未获取</span>
  }

  if (!quota.valid) {
    return <Badge variant="outline">HTTP {quota.status_code || "未知"}</Badge>
  }

  if (windows.length === 0) {
    return <Badge variant="secondary">已检查</Badge>
  }

  return (
    <div className="flex w-full min-w-0 flex-col gap-2 md:min-w-72">
      {windows.slice(0, 3).map((window) => (
        <Tooltip key={window.id}>
          <TooltipTrigger asChild>
            <div className="flex min-w-0 flex-col gap-1 rounded-lg border bg-muted/30 p-2">
              <div className="grid grid-cols-[3rem_1fr_3rem] items-center gap-2">
                <span className="font-mono text-xs text-muted-foreground">
                  {window.label}
                </span>
                {typeof window.usedPercent === "number" ? (
                  <Progress value={window.usedPercent} />
                ) : (
                  <span className="text-center text-[0.68rem] text-muted-foreground">
                    未返回使用率
                  </span>
                )}
                <span className="text-right font-mono text-xs">
                  {formatPercent(window.usedPercent)}
                </span>
              </div>
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1 pl-0 font-mono text-[0.68rem] leading-snug text-muted-foreground">
                <span>{quotaDetail(window)}</span>
                {window.resetAt ? (
                  <span>重置 {formatDate(window.resetAt)}</span>
                ) : null}
              </div>
            </div>
          </TooltipTrigger>
          <TooltipContent>
            <span>
              {quotaDetail(window)}
              {window.resetAt ? `, 重置 ${formatDateTime(window.resetAt)}` : ""}
            </span>
          </TooltipContent>
        </Tooltip>
      ))}
    </div>
  )
}
