package handler

import (
	"context"
	"encoding/json"
	"time"

	"codex-proxy/internal/auth"

	"github.com/valyala/fasthttp"
)

type accountIdentifierRequest struct {
	Email    string `json:"email"`
	FilePath string `json:"file_path"`
}

type accountUpdateRequest struct {
	Email    string                  `json:"email"`
	FilePath string                  `json:"file_path"`
	Account  auth.AccountUpdatePatch `json:"account"`
}

type accountDeleteRequest struct {
	Email    string `json:"email"`
	FilePath string `json:"file_path"`
	Hard     *bool  `json:"hard,omitempty"`
}

func (h *ProxyHandler) handleAdminAccountsCreate(ctx *fasthttp.RequestCtx) {
	body := ctx.PostBody()
	if len(body) == 0 {
		writeJSON(ctx, fasthttp.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "请求体不能为空"},
		})
		return
	}
	res, err := h.manager.IngestAccountsFromJSON(body)
	if err != nil {
		writeJSON(ctx, fasthttp.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error()},
		})
		return
	}
	writeJSON(ctx, fasthttp.StatusOK, res)
}

func (h *ProxyHandler) handleAdminAccountsUpdate(ctx *fasthttp.RequestCtx) {
	var req accountUpdateRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		writeJSON(ctx, fasthttp.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "JSON 解析失败"},
		})
		return
	}
	acc, err := h.manager.UpdateAccount(req.Email, req.FilePath, req.Account)
	if err != nil {
		writeJSON(ctx, fasthttp.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error()},
		})
		return
	}
	writeJSON(ctx, fasthttp.StatusOK, map[string]any{
		"object":  "account",
		"account": acc.GetStats(),
	})
}

func (h *ProxyHandler) handleAdminAccountsDelete(ctx *fasthttp.RequestCtx) {
	var req accountDeleteRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		writeJSON(ctx, fasthttp.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "JSON 解析失败"},
		})
		return
	}
	hard := true
	if req.Hard != nil {
		hard = *req.Hard
	}
	acc, err := h.manager.DeleteAccountByIdentifier(req.Email, req.FilePath, hard)
	if err != nil {
		writeJSON(ctx, fasthttp.StatusNotFound, map[string]any{
			"error": map[string]string{"message": err.Error()},
		})
		return
	}
	writeJSON(ctx, fasthttp.StatusOK, map[string]any{
		"object":    "account_delete_result",
		"email":     acc.GetEmail(),
		"file_path": acc.FilePath,
		"hard":      hard,
	})
}

func (h *ProxyHandler) handleAdminAccountsProbe(ctx *fasthttp.RequestCtx) {
	var req accountIdentifierRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		writeJSON(ctx, fasthttp.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "JSON 解析失败"},
		})
		return
	}
	acc := h.manager.FindAccountByIdentifier(req.Email, req.FilePath)
	if acc == nil {
		writeJSON(ctx, fasthttp.StatusNotFound, map[string]any{
			"error": map[string]string{"message": "未找到账号"},
		})
		return
	}

	probeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	verdict, httpStatus := h.quotaChecker.CheckAccountResultWithStatus(probeCtx, acc)
	status, message := probeStatus(verdict, httpStatus)
	writeJSON(ctx, fasthttp.StatusOK, map[string]any{
		"object":      "account_probe_result",
		"email":       acc.GetEmail(),
		"file_path":   acc.FilePath,
		"status":      status,
		"verdict":     verdict,
		"http_status": httpStatus,
		"message":     message,
		"checked_at":  time.Now().Format(time.RFC3339),
		"quota":       acc.GetStats().Quota,
	})
}

func probeStatus(verdict, httpStatus int) (string, string) {
	switch verdict {
	case 1:
		return "ok", "账号可用，额度接口返回有效"
	case 2:
		return "rate_limited", "额度接口返回 429"
	case -1:
		if httpStatus == 0 {
			return "invalid", "账号缺少 access token 或无法完成鉴权"
		}
		return "invalid", "额度接口返回非 200"
	default:
		if httpStatus >= 500 {
			return "transient_failed", "上游暂态错误"
		}
		if httpStatus <= 0 {
			return "transient_failed", "网络请求失败"
		}
		return "transient_failed", "测活未通过"
	}
}
