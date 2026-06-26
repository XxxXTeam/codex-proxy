package auth

import (
	"fmt"
	"strings"
	"time"
)

/* AccountUpdatePatch carries optional account edits. Nil means keep existing. */
type AccountUpdatePatch struct {
	Email         *string `json:"email,omitempty"`
	AccountID     *string `json:"account_id,omitempty"`
	IDToken       *string `json:"id_token,omitempty"`
	AccessToken   *string `json:"access_token,omitempty"`
	RefreshToken  *string `json:"refresh_token,omitempty"`
	RK            *string `json:"rk,omitempty"`
	Expired       *string `json:"expired,omitempty"`
	PlanType      *string `json:"plan_type,omitempty"`
	Status        *string `json:"status,omitempty"`
	DisableReason *string `json:"disable_reason,omitempty"`
	CooldownUntil *string `json:"cooldown_until,omitempty"`
}

func trimPtr(v *string) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
}

func parseOptionalRFC3339(v *string) (time.Time, error) {
	raw := trimPtr(v)
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("时间必须为 RFC3339: %w", err)
	}
	return t, nil
}

func (m *Manager) UpdateAccount(identifierEmail, identifierFilePath string, patch AccountUpdatePatch) (*Account, error) {
	acc := m.FindAccountByIdentifier(identifierEmail, identifierFilePath)
	if acc == nil {
		return nil, fmt.Errorf("未找到账号")
	}

	cooldownUntil, err := parseOptionalRFC3339(patch.CooldownUntil)
	if err != nil {
		return nil, err
	}

	acc.mu.Lock()
	if patch.Email != nil {
		acc.Token.Email = trimPtr(patch.Email)
	}
	if patch.AccountID != nil {
		acc.Token.AccountID = trimPtr(patch.AccountID)
	}
	if patch.IDToken != nil {
		acc.Token.IDToken = trimPtr(patch.IDToken)
	}
	if patch.AccessToken != nil {
		acc.Token.AccessToken = trimPtr(patch.AccessToken)
	}
	if patch.RefreshToken != nil {
		acc.Token.RefreshToken = trimPtr(patch.RefreshToken)
	}
	if patch.RK != nil {
		acc.Token.RefreshToken = trimPtr(patch.RK)
	}
	if patch.Expired != nil {
		acc.Token.Expire = trimPtr(patch.Expired)
	}
	if patch.PlanType != nil {
		acc.Token.PlanType = trimPtr(patch.PlanType)
	}

	status := strings.ToLower(trimPtr(patch.Status))
	disableReason := trimPtr(patch.DisableReason)
	switch status {
	case "":
	case "active":
		acc.Status = StatusActive
		acc.LastError = nil
		acc.ConsecutiveFailures = 0
		acc.DisableReason = ReasonNone
		acc.CooldownUntil = time.Time{}
		acc.QuotaExhausted = false
		acc.QuotaResetsAt = time.Time{}
	case "disabled":
		acc.Status = StatusDisabled
		acc.DisableReason = disableReason
	case "cooldown":
		acc.Status = StatusCooldown
		if !cooldownUntil.IsZero() {
			acc.CooldownUntil = cooldownUntil
		}
		if disableReason != "" {
			acc.DisableReason = disableReason
		}
	default:
		acc.mu.Unlock()
		return nil, fmt.Errorf("不支持的状态: %s", status)
	}
	if status == "" && patch.DisableReason != nil {
		acc.DisableReason = disableReason
	}
	acc.mu.Unlock()

	acc.SyncAccessExpireFromToken()
	acc.mu.RLock()
	acc.atomicStatus.Store(int32(acc.Status))
	if acc.CooldownUntil.IsZero() {
		acc.atomicCooldownMs.Store(0)
	} else {
		acc.atomicCooldownMs.Store(acc.CooldownUntil.UnixMilli())
	}
	acc.mu.RUnlock()

	if err := m.saveTokenToFile(acc); err != nil {
		return nil, err
	}
	m.InvalidateSelectorCache()
	return acc, nil
}

func (m *Manager) DeleteAccountByIdentifier(email, filePath string, hard bool) (*Account, error) {
	acc := m.FindAccountByIdentifier(email, filePath)
	if acc == nil {
		return nil, fmt.Errorf("未找到账号")
	}
	if hard {
		m.RemoveAccount(acc, "admin_delete")
	} else {
		m.DisableAccountByRenamingFile(acc, "admin_delete")
	}
	return acc, nil
}
