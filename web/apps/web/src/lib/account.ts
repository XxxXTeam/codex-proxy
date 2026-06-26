import type { AccountStats } from "@/lib/api"

export function accountKey(account: AccountStats) {
  return account.file_path || account.account_id || account.email || account.token_expire || "unknown"
}

export function statusLabel(status: string) {
  switch (status) {
    case "active":
      return "可用"
    case "cooldown":
      return "冷却"
    case "disabled":
      return "禁用"
    default:
      return status || "未知"
  }
}

export function statusVariant(status: string) {
  if (status === "active") return "default"
  if (status === "disabled") return "outline"
  return "secondary"
}
