import { type FormEvent, useEffect, useState } from "react"
import { Plus, Save, X } from "lucide-react"

import { Alert, AlertDescription, AlertTitle } from "@workspace/ui/components/alert"
import { Button } from "@workspace/ui/components/button"
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui/components/card"
import {
  Field,
  FieldDescription,
  FieldGroup,
  FieldLabel,
} from "@workspace/ui/components/field"
import { Input } from "@workspace/ui/components/input"

import {
  type AccountPatch,
  type AccountStats,
  type Credentials,
  createAccount,
  updateAccount,
} from "@/lib/api"

type AccountFormState = {
  email: string
  accountId: string
  planType: string
  status: string
  disableReason: string
  accessToken: string
  refreshToken: string
  idToken: string
  expired: string
}

function stateFromAccount(account: AccountStats | null): AccountFormState {
  return {
    email: account?.email ?? "",
    accountId: account?.account_id ?? "",
    planType: account?.plan_type ?? "",
    status: account?.status ?? "active",
    disableReason: account?.disable_reason ?? "",
    accessToken: "",
    refreshToken: "",
    idToken: "",
    expired: account?.token_expire ?? "",
  }
}

function patchFromState(state: AccountFormState, editing: boolean): AccountPatch {
  const patch: AccountPatch = {
    email: state.email.trim(),
    account_id: state.accountId.trim(),
    plan_type: state.planType.trim(),
    status: state.status.trim(),
    disable_reason: state.disableReason.trim(),
    expired: state.expired.trim(),
  }
  if (!editing || state.accessToken.trim()) patch.access_token = state.accessToken.trim()
  if (!editing || state.refreshToken.trim()) {
    patch.refresh_token = state.refreshToken.trim()
  }
  if (!editing || state.idToken.trim()) patch.id_token = state.idToken.trim()
  return patch
}

export function AccountEditor({
  credentials,
  account,
  onSaved,
  onClose,
}: {
  credentials: Credentials
  account: AccountStats | null
  onSaved: () => void
  onClose: () => void
}) {
  const [form, setForm] = useState<AccountFormState>(() => stateFromAccount(account))
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState("")
  const editing = Boolean(account)

  useEffect(() => {
    setForm(stateFromAccount(account))
    setError("")
  }, [account])

  function updateField<K extends keyof AccountFormState>(
    key: K,
    value: AccountFormState[K]
  ) {
    setForm((current) => ({ ...current, [key]: value }))
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setError("")
    setSubmitting(true)
    try {
      const patch = patchFromState(form, editing)
      if (!patch.email && !patch.account_id && !account?.file_path) {
        throw new Error("请至少填写邮箱或 Account ID")
      }
      if (editing && account) {
        await updateAccount(credentials, account, patch)
      } else {
        await createAccount(credentials, { ...patch, rk: patch.refresh_token })
      }
      onSaved()
      onClose()
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "保存账号失败")
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Card>
      <CardHeader className="gap-3">
        <div>
          <CardTitle>{editing ? "编辑账号" : "添加账号"}</CardTitle>
          <CardDescription>
            {editing
              ? "只提交有变化的基础信息；token 输入框留空会保留现有值。"
              : "至少填写 refresh token、access token 或 id token 之一。"}
          </CardDescription>
        </div>
        <CardAction>
          <Button variant="ghost" size="sm" onClick={onClose}>
            <X data-icon="inline-start" />
            关闭
          </Button>
        </CardAction>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <form className="flex flex-col gap-4" onSubmit={handleSubmit}>
          <FieldGroup className="grid gap-4 lg:grid-cols-4">
            <Field>
              <FieldLabel htmlFor="account-email">邮箱</FieldLabel>
              <Input
                id="account-email"
                value={form.email}
                onChange={(event) => updateField("email", event.target.value)}
                placeholder="name@example.test"
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="account-id">Account ID</FieldLabel>
              <Input
                id="account-id"
                value={form.accountId}
                onChange={(event) => updateField("accountId", event.target.value)}
                placeholder="acct_..."
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="account-plan">套餐</FieldLabel>
              <Input
                id="account-plan"
                value={form.planType}
                onChange={(event) => updateField("planType", event.target.value)}
                placeholder="plus / pro / team"
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="account-status">状态</FieldLabel>
              <select
                id="account-status"
                className="h-8 rounded-lg border border-input bg-background px-2 text-sm"
                value={form.status}
                onChange={(event) => updateField("status", event.target.value)}
              >
                <option value="active">可用</option>
                <option value="cooldown">冷却</option>
                <option value="disabled">禁用</option>
              </select>
            </Field>
          </FieldGroup>

          <FieldGroup className="grid gap-4 lg:grid-cols-2">
            <Field>
              <FieldLabel htmlFor="account-expired">Access token 过期时间</FieldLabel>
              <Input
                id="account-expired"
                value={form.expired}
                onChange={(event) => updateField("expired", event.target.value)}
                placeholder="2026-06-26T12:00:00Z"
              />
              <FieldDescription>使用 RFC3339；留空表示未知。</FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="account-disable-reason">禁用/冷却原因</FieldLabel>
              <Input
                id="account-disable-reason"
                value={form.disableReason}
                onChange={(event) =>
                  updateField("disableReason", event.target.value)
                }
                placeholder="可留空"
              />
            </Field>
          </FieldGroup>

          <FieldGroup className="grid gap-4 lg:grid-cols-3">
            <Field>
              <FieldLabel htmlFor="account-refresh-token">Refresh token / RK</FieldLabel>
              <Input
                id="account-refresh-token"
                value={form.refreshToken}
                onChange={(event) =>
                  updateField("refreshToken", event.target.value)
                }
                placeholder={editing ? "留空保留现有值" : "rk 或 refresh token"}
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="account-access-token">Access token</FieldLabel>
              <Input
                id="account-access-token"
                value={form.accessToken}
                onChange={(event) => updateField("accessToken", event.target.value)}
                placeholder={editing ? "留空保留现有值" : "可选"}
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="account-id-token">ID token</FieldLabel>
              <Input
                id="account-id-token"
                value={form.idToken}
                onChange={(event) => updateField("idToken", event.target.value)}
                placeholder={editing ? "留空保留现有值" : "可选"}
              />
            </Field>
          </FieldGroup>

          <div className="flex flex-wrap items-center gap-2">
            <Button type="submit" disabled={submitting}>
              {editing ? <Save data-icon="inline-start" /> : <Plus data-icon="inline-start" />}
              {submitting ? "保存中" : editing ? "保存修改" : "添加账号"}
            </Button>
            <Button type="button" variant="outline" onClick={onClose} disabled={submitting}>
              取消
            </Button>
          </div>
        </form>

        {error ? (
          <Alert variant="destructive">
            <AlertTitle>保存失败</AlertTitle>
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
      </CardContent>
    </Card>
  )
}
