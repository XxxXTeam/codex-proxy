import type { AccountStats, StatsResponse } from "@/lib/api"
import { parseQuotaWindows, primaryQuotaPercent } from "@/lib/quota"

export type ChartDatum = {
  label: string
  value: number
  detail?: string
}

function addToMap(map: Map<string, number>, key: string, value = 1) {
  map.set(key, (map.get(key) ?? 0) + value)
}

function mapToData(map: Map<string, number>) {
  return [...map.entries()]
    .map(([label, value]) => ({ label, value }))
    .sort((a, b) => b.value - a.value || a.label.localeCompare(b.label))
}

export function averageQuotaPercent(accounts: AccountStats[]) {
  const values = accounts
    .map((account) => primaryQuotaPercent(account.quota?.raw_data))
    .filter((value): value is number => typeof value === "number")
  if (values.length === 0) return undefined
  return values.reduce((sum, value) => sum + value, 0) / values.length
}

export function buildDashboard(stats: StatsResponse | null) {
  const accounts = stats?.accounts ?? []
  const summary = stats?.summary
  const statusData: ChartDatum[] = [
    { label: "可用", value: summary?.active ?? 0 },
    { label: "冷却", value: summary?.cooldown ?? 0 },
    { label: "禁用", value: summary?.disabled ?? 0 },
  ]

  const planMap = new Map<string, number>()
  const quotaBandMap = new Map<string, number>([
    ["0-49%", 0],
    ["50-79%", 0],
    ["80-99%", 0],
    ["100%", 0],
    ["未知", 0],
  ])
  const windowMap = new Map<string, number>()

  for (const account of accounts) {
    addToMap(planMap, account.plan_type || "unknown")
    const primary = primaryQuotaPercent(account.quota?.raw_data)
    if (typeof primary !== "number") addToMap(quotaBandMap, "未知")
    else if (primary >= 100) addToMap(quotaBandMap, "100%")
    else if (primary >= 80) addToMap(quotaBandMap, "80-99%")
    else if (primary >= 50) addToMap(quotaBandMap, "50-79%")
    else addToMap(quotaBandMap, "0-49%")

    for (const window of parseQuotaWindows(account.quota?.raw_data)) {
      addToMap(windowMap, window.label)
    }
  }

  const topUsage = [...accounts]
    .sort((a, b) => b.usage.total_tokens - a.usage.total_tokens)
    .slice(0, 6)
    .map((account) => ({
      label: account.email || account.account_id || "未知账号",
      value: account.usage.total_tokens,
      detail: account.plan_type || account.status,
    }))

  const errorRates = [...accounts]
    .filter((account) => account.total_requests > 0 || account.total_errors > 0)
    .sort((a, b) => b.total_errors - a.total_errors)
    .slice(0, 6)
    .map((account) => ({
      label: account.email || account.account_id || "未知账号",
      value:
        account.total_requests > 0
          ? (account.total_errors / account.total_requests) * 100
          : account.total_errors,
      detail: `${account.total_errors} errors`,
    }))

  const tokenMix = [
    { label: "输入", value: summary?.total_input_tokens ?? 0 },
    { label: "输出", value: summary?.total_output_tokens ?? 0 },
  ]

  return {
    statusData,
    planData: mapToData(planMap),
    quotaBands: mapToData(quotaBandMap),
    windowData: mapToData(windowMap),
    topUsage,
    errorRates,
    tokenMix,
    averageQuota: averageQuotaPercent(accounts),
    checkedCount: accounts.filter((account) => account.quota).length,
  }
}
