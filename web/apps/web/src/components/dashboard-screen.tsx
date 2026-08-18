import { Activity, Boxes, Clock, Database, RefreshCw, ShieldCheck } from "lucide-react"

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

import type { StatsResponse } from "@/lib/api"
import { buildDashboard } from "@/lib/dashboard"
import { formatDateTime, formatNumber, formatPercent } from "@/lib/format"
import { DonutChart, HorizontalBars, MetricBars } from "@/components/simple-charts"

function SummaryCard({
  title,
  value,
  detail,
  icon: Icon,
}: {
  title: string
  value: string
  detail: string
  icon: typeof Activity
}) {
  return (
    <Card size="sm" className="min-h-28">
      <CardHeader>
        <CardTitle className="text-sm text-muted-foreground">{title}</CardTitle>
        <CardAction>
          <div className="flex size-8 items-center justify-center rounded-lg bg-muted text-muted-foreground">
            <Icon />
          </div>
        </CardAction>
      </CardHeader>
      <CardContent className="flex flex-col gap-1">
        <div className="truncate font-heading text-2xl font-semibold tabular-nums sm:text-3xl">
          {value}
        </div>
        <div className="text-xs leading-snug text-muted-foreground">{detail}</div>
      </CardContent>
    </Card>
  )
}

export function DashboardScreen({
  stats,
  onRefreshCatalog,
  catalogRefreshing = false,
}: {
  stats: StatsResponse | null
  onRefreshCatalog?: () => void
  catalogRefreshing?: boolean
}) {
  const dashboard = buildDashboard(stats)
  const totalAccounts = stats?.summary.total ?? 0
  const catalog = stats?.catalog

  return (
    <section className="flex flex-col gap-3">
      <div className="grid grid-cols-2 gap-3 xl:grid-cols-6">
        <SummaryCard
          title="账号总数"
          value={formatNumber(totalAccounts)}
          detail={`${formatNumber(stats?.summary.active)} 个可用账号`}
          icon={Database}
        />
        <SummaryCard
          title="冷却与禁用"
          value={formatNumber(
            (stats?.summary.cooldown ?? 0) + (stats?.summary.disabled ?? 0)
          )}
          detail={`${formatNumber(stats?.summary.cooldown)} 冷却, ${formatNumber(stats?.summary.disabled)} 禁用`}
          icon={Clock}
        />
        <SummaryCard
          title="请求速率"
          value={formatNumber(stats?.summary.rpm)}
          detail="最近一分钟 RPM"
          icon={Activity}
        />
        <SummaryCard
          title="额度使用"
          value={
            dashboard.averageQuota === undefined
              ? "未知"
              : formatPercent(dashboard.averageQuota)
          }
          detail={`${formatNumber(stats?.summary.quota_checked ?? dashboard.checkedCount)} 已检查 / ${formatNumber(stats?.summary.quota_exhausted)} 用尽`}
          icon={ShieldCheck}
        />
        <SummaryCard
          title="Token 总量"
          value={formatNumber(stats?.summary.total_tokens)}
          detail={`输入 ${formatNumber(stats?.summary.total_input_tokens)} / 输出 ${formatNumber(stats?.summary.total_output_tokens)} · 缓存读 ${formatNumber(stats?.summary.total_cache_read_tokens)} · 推理 ${formatNumber(stats?.summary.total_reasoning_tokens)}`}
          icon={Activity}
        />
        <SummaryCard
          title="额度健康"
          value={formatNumber(stats?.summary.quota_valid)}
          detail={`${formatNumber(stats?.summary.quota_invalid)} 个快照无效`}
          icon={ShieldCheck}
        />
      </div>

      <Card>
        <CardHeader className="gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <CardTitle>模型目录与客户端</CardTitle>
            <CardDescription>
              服务端缓存远端 Codex 客户端模型目录，后台按固定周期刷新。
            </CardDescription>
          </div>
          <CardAction>
            <Button
              variant="outline"
              size="sm"
              onClick={onRefreshCatalog}
              disabled={!onRefreshCatalog || catalogRefreshing}
            >
              <RefreshCw data-icon="inline-start" />
              {catalogRefreshing ? "更新中" : "立即更新"}
            </Button>
          </CardAction>
        </CardHeader>
        <CardContent className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
          <div>
            <div className="text-xs text-muted-foreground">客户端版本</div>
            <div className="mt-1 font-mono text-sm font-semibold">
              {catalog?.client_version || "未知"}
            </div>
          </div>
          <div>
            <div className="text-xs text-muted-foreground">模型数量</div>
            <div className="mt-1 flex items-center gap-2 font-mono text-sm font-semibold">
              <Boxes className="size-4 text-muted-foreground" />
              {formatNumber(catalog?.model_count)}
            </div>
          </div>
          <div>
            <div className="text-xs text-muted-foreground">目录 revision</div>
            <div className="mt-1 font-mono text-sm font-semibold">
              {formatNumber(catalog?.revision)}
            </div>
          </div>
          <div>
            <div className="text-xs text-muted-foreground">最近成功更新</div>
            <div className="mt-1 text-sm font-semibold">
              {catalog?.updated_at ? formatDateTime(catalog.updated_at) : "未记录"}
            </div>
          </div>
          <div>
            <div className="text-xs text-muted-foreground">刷新策略</div>
            <div className="mt-1 text-sm font-semibold">
              {catalog?.refresh_interval_sec
                ? `${Math.round(catalog.refresh_interval_sec / 3600)} 小时`
                : "未知"}
            </div>
            {catalog?.last_error ? (
              <Badge variant="destructive" className="mt-1 max-w-full truncate">
                刷新失败
              </Badge>
            ) : (
              <Badge variant="secondary" className="mt-1">
                {catalog?.source || "本地缓存"}
              </Badge>
            )}
          </div>
        </CardContent>
      </Card>

      <div className="grid gap-3 xl:grid-cols-[1.05fr_0.95fr]">
        <Card>
          <CardHeader>
            <CardTitle>数据大屏</CardTitle>
            <CardDescription>状态、套餐、额度和 token 使用的实时快照。</CardDescription>
          </CardHeader>
          <CardContent className="grid gap-5 lg:grid-cols-[0.9fr_1.1fr]">
            <DonutChart
              data={dashboard.statusData}
              center={formatNumber(totalAccounts)}
            />
            <MetricBars data={dashboard.tokenMix} />
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>套餐分布</CardTitle>
            <CardDescription>按账号当前 plan_type 汇总。</CardDescription>
          </CardHeader>
          <CardContent>
            <HorizontalBars data={dashboard.planData} />
          </CardContent>
        </Card>
      </div>

      <div className="grid gap-3 xl:grid-cols-4">
        <Card className="xl:col-span-2">
          <CardHeader>
            <CardTitle>Token 消耗 Top</CardTitle>
            <CardDescription>当前页账号按总 token 排序。</CardDescription>
          </CardHeader>
          <CardContent>
            <HorizontalBars data={dashboard.topUsage} valueKind="compact" />
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>额度压力</CardTitle>
            <CardDescription>主窗口使用率分段。</CardDescription>
          </CardHeader>
          <CardContent>
            <HorizontalBars data={dashboard.quotaBands} />
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>窗口类型</CardTitle>
            <CardDescription>自动识别 5h、7d 等不同窗口。</CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            {dashboard.windowData.length ? (
              dashboard.windowData.slice(0, 8).map((item) => (
                <div key={item.label} className="flex items-center justify-between gap-3">
                  <Badge variant="secondary">{item.label}</Badge>
                  <span className="font-mono text-sm">{formatNumber(item.value)}</span>
                </div>
              ))
            ) : (
              <div className="flex min-h-32 items-center justify-center text-sm text-muted-foreground">
                暂无额度窗口
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>错误率观察</CardTitle>
          <CardDescription>优先展示错误数较高的账号，便于快速查询额度或清理。</CardDescription>
        </CardHeader>
        <CardContent>
          <HorizontalBars data={dashboard.errorRates} valueKind="percent" />
        </CardContent>
      </Card>
    </section>
  )
}
