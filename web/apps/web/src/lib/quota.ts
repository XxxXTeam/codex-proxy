export type QuotaWindow = {
  id: string
  label: string
  usedPercent?: number
  used?: number
  limit?: number
  remaining?: number
  resetAt?: string
}

const WINDOW_HINTS = [
  "window",
  "weekly",
  "daily",
  "hourly",
  "rate_limit",
  "limit",
  "quota",
  "usage",
]

const USED_PERCENT_KEYS = [
  "used_percent",
  "usedPercent",
  "usage_percent",
  "usagePercent",
  "percent_used",
  "percentUsed",
  "percentage",
  "percent",
]

const RESET_KEYS = [
  "reset_at",
  "resetAt",
  "resets_at",
  "resetsAt",
  "reset_time",
  "resetTime",
  "expires_at",
  "expiresAt",
]

const DURATION_KEYS = [
  "duration",
  "window",
  "period",
  "seconds",
  "duration_seconds",
  "durationSeconds",
  "window_seconds",
  "windowSeconds",
]

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value)
}

function normalizeRawData(rawData: unknown): unknown {
  if (typeof rawData !== "string") return rawData
  try {
    return JSON.parse(rawData)
  } catch {
    return rawData
  }
}

function numberFrom(value: unknown): number | undefined {
  if (typeof value === "number" && Number.isFinite(value)) return value
  if (typeof value !== "string") return undefined
  const trimmed = value.trim()
  if (!trimmed) return undefined
  const parsed = Number(trimmed.replace("%", ""))
  return Number.isFinite(parsed) ? parsed : undefined
}

function stringFrom(value: unknown): string | undefined {
  if (typeof value === "string" && value.trim()) return value.trim()
  if (typeof value === "number" && Number.isFinite(value)) {
    if (value >= 1_000_000_000_000) return new Date(value).toISOString()
    if (value >= 1_000_000_000) return new Date(value * 1000).toISOString()
  }
  return undefined
}

function valueByKeys(record: Record<string, unknown>, keys: string[]) {
  for (const key of keys) {
    if (key in record) return record[key]
  }
  return undefined
}

function secondsToLabel(seconds: number) {
  if (!Number.isFinite(seconds) || seconds <= 0) return undefined
  if (seconds % 604800 === 0) return `${seconds / 604800}w`
  if (seconds % 86400 === 0) return `${seconds / 86400}d`
  if (seconds % 3600 === 0) return `${seconds / 3600}h`
  if (seconds % 60 === 0) return `${seconds / 60}m`
  return `${seconds}s`
}

function durationLabel(value: unknown) {
  const n = numberFrom(value)
  if (typeof n === "number") return secondsToLabel(n)
  if (typeof value !== "string") return undefined
  const trimmed = value.trim().toLowerCase()
  if (/^\d+\s*(s|sec|secs|second|seconds)$/.test(trimmed)) {
    return secondsToLabel(Number.parseInt(trimmed, 10))
  }
  if (/^\d+\s*(m|min|mins|minute|minutes)$/.test(trimmed)) {
    return `${Number.parseInt(trimmed, 10)}m`
  }
  if (/^\d+\s*(h|hr|hrs|hour|hours)$/.test(trimmed)) {
    return `${Number.parseInt(trimmed, 10)}h`
  }
  if (/^\d+\s*(d|day|days)$/.test(trimmed)) {
    return `${Number.parseInt(trimmed, 10)}d`
  }
  if (/^\d+\s*(w|week|weeks)$/.test(trimmed)) {
    return `${Number.parseInt(trimmed, 10) * 7}d`
  }
  return undefined
}

function readableKey(key: string) {
  return key
    .replace(/([a-z])([A-Z])/g, "$1 $2")
    .replace(/[_-]+/g, " ")
    .replace(/\s+/g, " ")
    .trim()
    .toLowerCase()
}

function inferLabel(path: string[], record: Record<string, unknown>) {
  for (const key of DURATION_KEYS) {
    const label = durationLabel(record[key])
    if (label) return label
  }

  const joined = path.join(".").toLowerCase()
  if (joined.includes("weekly") || joined.includes("secondary")) return "7d"
  if (joined.includes("daily")) return "1d"
  if (joined.includes("hourly")) return "1h"

  const lastMeaningful = [...path]
    .reverse()
    .find((part) => part && !["rate_limit", "quota", "usage"].includes(part))
  return lastMeaningful ? readableKey(lastMeaningful) : "quota"
}

function looksLikeWindow(path: string[], record: Record<string, unknown>) {
  const keys = Object.keys(record)
  const lowerPath = path.join(".").toLowerCase()
  return (
    USED_PERCENT_KEYS.some((key) => key in record) ||
    ["limit", "used", "remaining"].some((key) => key in record) ||
    RESET_KEYS.some((key) => key in record) ||
    DURATION_KEYS.some((key) => key in record) ||
    WINDOW_HINTS.some((hint) => lowerPath.includes(hint)) ||
    keys.some((key) => WINDOW_HINTS.some((hint) => key.toLowerCase().includes(hint)))
  )
}

function windowFromRecord(
  path: string[],
  record: Record<string, unknown>
): QuotaWindow | null {
  if (!looksLikeWindow(path, record)) return null

  const usedPercent = numberFrom(valueByKeys(record, USED_PERCENT_KEYS))
  const used = numberFrom(record.used ?? record.current ?? record.consumed)
  const limit = numberFrom(record.limit ?? record.max ?? record.total)
  const remaining = numberFrom(record.remaining ?? record.left ?? record.available)
  const resetAt = stringFrom(valueByKeys(record, RESET_KEYS))
  const computedPercent =
    typeof usedPercent === "number"
      ? usedPercent
      : typeof used === "number" && typeof limit === "number" && limit > 0
        ? (used / limit) * 100
        : undefined

  if (
    typeof computedPercent !== "number" &&
    typeof used !== "number" &&
    typeof limit !== "number" &&
    typeof remaining !== "number" &&
    !resetAt
  ) {
    return null
  }

  const label = inferLabel(path, record)
  return {
    id: `${path.join(".")}:${label}`,
    label,
    usedPercent:
      typeof computedPercent === "number"
        ? Math.max(0, Math.min(100, computedPercent))
        : undefined,
    used,
    limit,
    remaining,
    resetAt,
  }
}

export function parseQuotaWindows(rawData: unknown): QuotaWindow[] {
  const root = normalizeRawData(rawData)
  const windows: QuotaWindow[] = []
  const seen = new Set<string>()

  function visit(value: unknown, path: string[]) {
    if (windows.length >= 8) return
    if (Array.isArray(value)) {
      value.forEach((item, index) => visit(item, [...path, String(index)]))
      return
    }
    if (!isRecord(value)) return

    const window = windowFromRecord(path, value)
    if (window && !seen.has(window.id)) {
      seen.add(window.id)
      windows.push(window)
    }

    for (const [key, child] of Object.entries(value)) {
      if (isRecord(child) || Array.isArray(child)) visit(child, [...path, key])
    }
  }

  visit(root, [])
  return windows
}

export function primaryQuotaPercent(rawData: unknown) {
  const root = normalizeRawData(rawData)
  if (isRecord(root)) {
    const rateLimit = root.rate_limit
    if (isRecord(rateLimit) && isRecord(rateLimit.primary_window)) {
      const primary = windowFromRecord(
        ["rate_limit", "primary_window"],
        rateLimit.primary_window
      )
      if (typeof primary?.usedPercent === "number") {
        return primary.usedPercent
      }
    }
  }

  return parseQuotaWindows(root).find(
    (window) => typeof window.usedPercent === "number"
  )?.usedPercent
}
