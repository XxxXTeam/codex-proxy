import { formatCompact, formatNumber } from "@/lib/format"
import type { ChartDatum } from "@/lib/dashboard"

const chartClasses = [
  "bg-chart-1",
  "bg-chart-2",
  "bg-chart-3",
  "bg-chart-4",
  "bg-chart-5",
]

const chartFills = [
  "var(--chart-1)",
  "var(--chart-2)",
  "var(--chart-3)",
  "var(--chart-4)",
  "var(--chart-5)",
]

function totalOf(data: ChartDatum[]) {
  return data.reduce((sum, item) => sum + Math.max(0, item.value), 0)
}

export function DonutChart({
  data,
  center,
}: {
  data: ChartDatum[]
  center: string
}) {
  const total = Math.max(1, totalOf(data))
  const segments = data.map((item, index) => {
    const value = Math.max(0, item.value)
    const length = (value / total) * 100
    const previousLength = data
      .slice(0, index)
      .reduce((sum, previous) => sum + (Math.max(0, previous.value) / total) * 100, 0)
    return {
      item,
      index,
      length,
      offset: 25 - previousLength,
    }
  })

  return (
    <div className="flex items-center gap-4">
      <div className="relative size-32 shrink-0">
        <svg viewBox="0 0 42 42" className="size-32 -rotate-90">
          <circle
            cx="21"
            cy="21"
            r="15.915"
            fill="transparent"
            stroke="var(--muted)"
            strokeWidth="6"
          />
          {segments.map(({ item, index, length, offset }) => {
            const strokeDasharray = `${length} ${100 - length}`
            return (
              <circle
                key={item.label}
                cx="21"
                cy="21"
                r="15.915"
                fill="transparent"
                stroke={chartFills[index % chartFills.length]}
                strokeWidth="6"
                strokeDasharray={strokeDasharray}
                strokeDashoffset={offset}
              />
            )
          })}
        </svg>
        <div className="absolute inset-0 flex flex-col items-center justify-center text-center">
          <div className="font-mono text-xl font-semibold tabular-nums">{center}</div>
          <div className="text-xs text-muted-foreground">total</div>
        </div>
      </div>
      <div className="flex min-w-0 flex-1 flex-col gap-2">
        {data.map((item, index) => (
          <div key={item.label} className="flex min-w-0 items-center gap-2">
            <span
              className={`size-2 shrink-0 rounded-sm ${chartClasses[index % chartClasses.length]}`}
            />
            <span className="min-w-0 flex-1 truncate text-sm">{item.label}</span>
            <span className="font-mono text-sm tabular-nums">
              {formatNumber(item.value)}
            </span>
          </div>
        ))}
      </div>
    </div>
  )
}

export function HorizontalBars({
  data,
  valueKind = "number",
}: {
  data: ChartDatum[]
  valueKind?: "number" | "compact" | "percent"
}) {
  const max = Math.max(1, ...data.map((item) => item.value))

  return (
    <div className="flex flex-col gap-3">
      {data.length === 0 ? (
        <div className="flex min-h-32 items-center justify-center text-sm text-muted-foreground">
          暂无可展示数据
        </div>
      ) : null}
      {data.map((item, index) => {
        const value = Math.max(0, item.value)
        const width = Math.max(3, Math.round((value / max) * 100))
        const label =
          valueKind === "compact"
            ? formatCompact(value)
            : valueKind === "percent"
              ? `${value.toFixed(1)}%`
              : formatNumber(value)
        return (
          <div key={item.label} className="grid gap-1">
            <div className="flex min-w-0 items-center justify-between gap-3">
              <span className="min-w-0 truncate text-sm">{item.label}</span>
              <span className="shrink-0 font-mono text-xs text-muted-foreground">
                {label}
              </span>
            </div>
            <div className="h-2 overflow-hidden rounded-sm bg-muted">
              <div
                className={`h-full rounded-sm ${chartClasses[index % chartClasses.length]}`}
                style={{ width: `${width}%` }}
              />
            </div>
            {item.detail ? (
              <div className="truncate text-xs text-muted-foreground">{item.detail}</div>
            ) : null}
          </div>
        )
      })}
    </div>
  )
}

export function MetricBars({ data }: { data: ChartDatum[] }) {
  const total = Math.max(1, totalOf(data))
  return (
    <div className="flex flex-col gap-3">
      <div className="flex h-9 overflow-hidden rounded-lg bg-muted">
        {data.map((item, index) => {
          const width = Math.max(6, (item.value / total) * 100)
          return (
            <div
              key={item.label}
              className={chartClasses[index % chartClasses.length]}
              style={{ width: `${width}%` }}
              title={`${item.label}: ${formatCompact(item.value)}`}
            />
          )
        })}
      </div>
      <div className="grid grid-cols-2 gap-2">
        {data.map((item, index) => (
          <div key={item.label} className="rounded-lg bg-muted p-3">
            <div className="flex items-center gap-2 text-xs text-muted-foreground">
              <span
                className={`size-2 rounded-sm ${chartClasses[index % chartClasses.length]}`}
              />
              {item.label}
            </div>
            <div className="mt-1 truncate font-mono text-xl font-semibold tabular-nums">
              {formatCompact(item.value)}
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}
