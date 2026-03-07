/**
 * Codex 执行器模块
 * 负责向 Codex API 发送请求并处理响应
 * 支持流式和非流式两种模式，处理认证头注入、错误处理和重试
 */
package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"codex-proxy/internal/auth"
	"codex-proxy/internal/thinking"
	"codex-proxy/internal/translator"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

/* Codex 客户端版本常量，用于请求头 */
const (
	codexClientVersion = "0.101.0"
	codexUserAgent     = "codex_cli_rs/0.101.0 (Mac OS 26.0.1; arm64) Apple_Terminal/464"
)

/**
 * Executor Codex 请求执行器
 * 使用全局共享连接池提升高并发性能
 * @field baseURL - Codex API 基础 URL
 * @field httpClient - 共享的 HTTP 客户端（连接池复用）
 */
type Executor struct {
	baseURL       string
	httpClient    *http.Client
	keepAliveOnce sync.Once
}

/**
 * NewExecutor 创建新的 Codex 执行器
 * 初始化全局连接池，支持高并发场景
 * @param baseURL - API 基础 URL
 * @param proxyURL - 代理地址
 * @returns *Executor - 执行器实例
 */
func NewExecutor(baseURL, proxyURL string) *Executor {
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 50,
		MaxConnsPerHost:     100,
		IdleConnTimeout:     300 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: false},
	}

	if proxyURL != "" {
		proxyParsed, err := url.Parse(proxyURL)
		if err == nil {
			transport.Proxy = http.ProxyURL(proxyParsed)
		}
	}

	return &Executor{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Minute,
		},
	}
}

/**
 * ExecuteStream 执行流式请求
 * 将 OpenAI 格式请求转换为 Codex 格式，发送并以 SSE 方式返回响应
 *
 * @param ctx - 上下文
 * @param account - 使用的账号
 * @param requestBody - 原始 OpenAI Chat Completions 请求体
 * @param model - 模型名称（可能含思考后缀）
 * @param writer - HTTP 响应写入器
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteStream(ctx context.Context, account *auth.Account, requestBody []byte, model string, writer http.ResponseWriter) error {
	/* 应用思考配置并获取真实模型名 */
	body, baseModel := thinking.ApplyThinking(requestBody, model)

	/* 转换请求格式：OpenAI → Codex */
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)

	/* 设置模型和流式参数 */
	codexBody, _ = sjson.SetBytes(codexBody, "model", baseModel)
	codexBody, _ = sjson.DeleteBytes(codexBody, "previous_response_id")
	codexBody, _ = sjson.DeleteBytes(codexBody, "prompt_cache_retention")
	codexBody, _ = sjson.DeleteBytes(codexBody, "safety_identifier")
	/* #1901: 剔除 generate 参数，非 Codex 原生后端不支持此参数 */
	codexBody, _ = sjson.DeleteBytes(codexBody, "generate")
	if !gjson.GetBytes(codexBody, "instructions").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "instructions", "")
	}

	/* 构建 HTTP 请求 */
	apiURL := e.baseURL + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(codexBody))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}

	applyCodexHeaders(httpReq, account, true)

	/* 发送请求（使用全局连接池） */
	httpResp, err := e.httpClient.Do(httpReq)
	if err != nil {
		account.RecordFailure()
		return fmt.Errorf("请求发送失败: %w", err)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	/* 处理错误状态码 */
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(httpResp.Body)
		log.Errorf("Codex API 错误 [%d]: %s", httpResp.StatusCode, summarizeError(errBody))

		/* 根据状态码反馈账号状态 */
		handleAccountError(account, httpResp.StatusCode, errBody)

		return &StatusError{Code: httpResp.StatusCode, Body: errBody}
	}

	/* 设置 SSE 响应头 */
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)

	flusher, canFlush := writer.(http.Flusher)

	/* 构建反向工具名映射 */
	reverseToolMap := translator.BuildReverseToolNameMap(requestBody)
	state := translator.NewStreamState(baseModel)

	/* 逐行读取 SSE 事件并转换 */
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800)

	for scanner.Scan() {
		line := scanner.Bytes()
		/* 提取流式 usage */
		extractUsageFromStreamLine(line, account)
		chunks := translator.ConvertStreamChunk(ctx, line, state, reverseToolMap)
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(writer, "data: %s\n\n", chunk)
			if canFlush {
				flusher.Flush()
			}
		}
	}

	if err = scanner.Err(); err != nil {
		log.Errorf("读取流式响应失败: %v", err)
		return err
	}

	/* 发送结束标记 */
	_, _ = fmt.Fprintf(writer, "data: [DONE]\n\n")
	if canFlush {
		flusher.Flush()
	}

	return nil
}

/**
 * ExecuteNonStream 执行非流式请求
 * 将 OpenAI 格式请求转换为 Codex 格式，发送并返回完整响应
 *
 * @param ctx - 上下文
 * @param account - 使用的账号
 * @param requestBody - 原始 OpenAI Chat Completions 请求体
 * @param model - 模型名称（可能含思考后缀）
 * @returns []byte - OpenAI Chat Completions 格式的响应 JSON
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteNonStream(ctx context.Context, account *auth.Account, requestBody []byte, model string) ([]byte, error) {
	/* 应用思考配置 */
	body, baseModel := thinking.ApplyThinking(requestBody, model)

	/* 转换请求格式 */
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	codexBody, _ = sjson.SetBytes(codexBody, "model", baseModel)
	codexBody, _ = sjson.SetBytes(codexBody, "stream", true)
	codexBody, _ = sjson.DeleteBytes(codexBody, "previous_response_id")
	codexBody, _ = sjson.DeleteBytes(codexBody, "prompt_cache_retention")
	codexBody, _ = sjson.DeleteBytes(codexBody, "safety_identifier")
	/* #1901: 剔除 generate 参数 */
	codexBody, _ = sjson.DeleteBytes(codexBody, "generate")
	if !gjson.GetBytes(codexBody, "instructions").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "instructions", "")
	}

	/* 构建并发送请求 */
	apiURL := e.baseURL + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(codexBody))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	applyCodexHeaders(httpReq, account, true)

	httpResp, err := e.httpClient.Do(httpReq)
	if err != nil {
		account.RecordFailure()
		return nil, fmt.Errorf("请求发送失败: %w", err)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(httpResp.Body)
		handleAccountError(account, httpResp.StatusCode, errBody)
		return nil, &StatusError{Code: httpResp.StatusCode, Body: errBody}
	}

	/*
	 * 边读边找 response.completed 事件，找到即返回
	 * 不等完整 SSE 流结束，大幅减少非流式请求的等待时间
	 */
	reverseToolMap := translator.BuildReverseToolNameMap(requestBody)
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800)

	for scanner.Scan() {
		line := scanner.Bytes()
		/* 提取流式 usage */
		extractUsageFromStreamLine(line, account)

		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		jsonData := bytes.TrimSpace(line[5:])
		if gjson.GetBytes(jsonData, "type").String() != "response.completed" {
			continue
		}
		result := translator.ConvertNonStreamResponse(ctx, jsonData, reverseToolMap)
		if result != "" {
			return []byte(result), nil
		}
	}

	if err = scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	return nil, fmt.Errorf("未收到 response.completed 事件")
}

/**
 * ExecuteResponsesStream 执行 Responses API 流式请求
 * 直接透传 Codex SSE 事件到客户端，不做 Chat Completions 格式转换
 *
 * @param ctx - 上下文
 * @param account - 使用的账号
 * @param requestBody - Responses API 格式的请求体
 * @param model - 模型名称（可能含思考后缀）
 * @param writer - HTTP 响应写入器
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteResponsesStream(ctx context.Context, account *auth.Account, requestBody []byte, model string, writer http.ResponseWriter) error {
	/* 应用思考配置并获取真实模型名 */
	body, baseModel := thinking.ApplyThinking(requestBody, model)

	/* Responses API 格式已经很接近 Codex 格式，使用通用转换器处理 */
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	codexBody, _ = sjson.SetBytes(codexBody, "model", baseModel)
	codexBody, _ = sjson.DeleteBytes(codexBody, "previous_response_id")
	codexBody, _ = sjson.DeleteBytes(codexBody, "prompt_cache_retention")
	codexBody, _ = sjson.DeleteBytes(codexBody, "safety_identifier")
	codexBody, _ = sjson.DeleteBytes(codexBody, "generate")
	if !gjson.GetBytes(codexBody, "instructions").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "instructions", "")
	}

	/* 构建 HTTP 请求 */
	apiURL := e.baseURL + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(codexBody))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}

	applyCodexHeaders(httpReq, account, true)

	/* 发送请求 */
	httpResp, err := e.httpClient.Do(httpReq)
	if err != nil {
		account.RecordFailure()
		return fmt.Errorf("请求发送失败: %w", err)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	/* 处理错误状态码 */
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(httpResp.Body)
		log.Errorf("Codex API 错误 [%d]: %s", httpResp.StatusCode, summarizeError(errBody))
		handleAccountError(account, httpResp.StatusCode, errBody)
		return &StatusError{Code: httpResp.StatusCode, Body: errBody}
	}

	/* 设置 SSE 响应头 */
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)

	flusher, canFlush := writer.(http.Flusher)

	/*
	 * 直接字节级实时转发，不经过 Scanner 缓冲
	 * 每读到一块数据立即写入客户端并 Flush，实现真正的实时转发
	 */
	buf := make([]byte, 8192)
	for {
		n, readErr := httpResp.Body.Read(buf)
		if n > 0 {
			_, _ = writer.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				log.Errorf("读取流式响应失败: %v", readErr)
				return readErr
			}
			break
		}
	}

	return nil
}

/**
 * ExecuteResponsesNonStream 执行 Responses API 非流式请求
 * 从 Codex SSE 响应中提取 response.completed 事件，返回原生 response 对象
 *
 * @param ctx - 上下文
 * @param account - 使用的账号
 * @param requestBody - Responses API 格式的请求体
 * @param model - 模型名称（可能含思考后缀）
 * @returns []byte - Codex Responses API 格式的完整响应 JSON
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteResponsesNonStream(ctx context.Context, account *auth.Account, requestBody []byte, model string) ([]byte, error) {
	/* 应用思考配置 */
	body, baseModel := thinking.ApplyThinking(requestBody, model)

	/* 转换请求格式 */
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	codexBody, _ = sjson.SetBytes(codexBody, "model", baseModel)
	codexBody, _ = sjson.SetBytes(codexBody, "stream", true)
	codexBody, _ = sjson.DeleteBytes(codexBody, "previous_response_id")
	codexBody, _ = sjson.DeleteBytes(codexBody, "prompt_cache_retention")
	codexBody, _ = sjson.DeleteBytes(codexBody, "safety_identifier")
	codexBody, _ = sjson.DeleteBytes(codexBody, "generate")
	if !gjson.GetBytes(codexBody, "instructions").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "instructions", "")
	}

	/* 构建并发送请求 */
	apiURL := e.baseURL + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(codexBody))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	applyCodexHeaders(httpReq, account, true)

	httpResp, err := e.httpClient.Do(httpReq)
	if err != nil {
		account.RecordFailure()
		return nil, fmt.Errorf("请求发送失败: %w", err)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(httpResp.Body)
		handleAccountError(account, httpResp.StatusCode, errBody)
		return nil, &StatusError{Code: httpResp.StatusCode, Body: errBody}
	}

	/*
	 * 边读边找 response.completed 事件，找到即返回
	 * 不等完整 SSE 流结束，大幅减少非流式请求的等待时间
	 */
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800)

	for scanner.Scan() {
		line := scanner.Bytes()
		extractUsageFromStreamLine(line, account)

		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		jsonData := bytes.TrimSpace(line[5:])
		if gjson.GetBytes(jsonData, "type").String() != "response.completed" {
			continue
		}
		/* 返回 response 对象（Responses API 原生格式） */
		if resp := gjson.GetBytes(jsonData, "response"); resp.Exists() {
			return []byte(resp.Raw), nil
		}
	}

	if err = scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	return nil, fmt.Errorf("未收到 response.completed 事件")
}

/**
 * ExecuteResponsesCompactStream 执行 Responses Compact API 流式请求
 * 使用 /responses/compact 端点，直接透传 Codex SSE 事件到客户端
 *
 * @param ctx - 上下文
 * @param account - 使用的账号
 * @param requestBody - Responses API 格式的请求体
 * @param model - 模型名称（可能含思考后缀）
 * @param writer - HTTP 响应写入器
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteResponsesCompactStream(ctx context.Context, account *auth.Account, requestBody []byte, model string, writer http.ResponseWriter) error {
	/* 应用思考配置并获取真实模型名 */
	body, baseModel := thinking.ApplyThinking(requestBody, model)

	/* Responses API 格式已经很接近 Codex 格式，使用通用转换器处理 */
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	codexBody, _ = sjson.SetBytes(codexBody, "model", baseModel)
	codexBody, _ = sjson.DeleteBytes(codexBody, "previous_response_id")
	codexBody, _ = sjson.DeleteBytes(codexBody, "prompt_cache_retention")
	codexBody, _ = sjson.DeleteBytes(codexBody, "safety_identifier")
	codexBody, _ = sjson.DeleteBytes(codexBody, "generate")
	if !gjson.GetBytes(codexBody, "instructions").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "instructions", "")
	}

	/* 构建 HTTP 请求 - 使用 /responses/compact 端点 */
	apiURL := e.baseURL + "/responses/compact"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(codexBody))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}

	applyCodexHeaders(httpReq, account, true)

	/* 发送请求 */
	httpResp, err := e.httpClient.Do(httpReq)
	if err != nil {
		account.RecordFailure()
		return fmt.Errorf("请求发送失败: %w", err)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	/* 处理错误状态码 */
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(httpResp.Body)
		log.Errorf("Codex Compact API 错误 [%d]: %s", httpResp.StatusCode, summarizeError(errBody))
		handleAccountError(account, httpResp.StatusCode, errBody)
		return &StatusError{Code: httpResp.StatusCode, Body: errBody}
	}

	/* 透传响应头 */
	for k, vs := range httpResp.Header {
		for _, v := range vs {
			writer.Header().Add(k, v)
		}
	}
	writer.WriteHeader(http.StatusOK)

	flusher, canFlush := writer.(http.Flusher)

	/* 直接透传响应体（SSE 或 CBOR 等 compact 格式） */
	buf := make([]byte, 32*1024)
	for {
		n, readErr := httpResp.Body.Read(buf)
		if n > 0 {
			_, _ = writer.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}

	return nil
}

/**
 * ExecuteResponsesCompactNonStream 执行 Responses Compact API 非流式请求
 * 使用 /responses/compact 端点，返回 compact 格式的完整响应
 *
 * @param ctx - 上下文
 * @param account - 使用的账号
 * @param requestBody - Responses API 格式的请求体
 * @param model - 模型名称（可能含思考后缀）
 * @returns []byte - compact 格式的完整响应
 * @returns error - 执行失败时返回错误
 */
func (e *Executor) ExecuteResponsesCompactNonStream(ctx context.Context, account *auth.Account, requestBody []byte, model string) ([]byte, error) {
	/* 应用思考配置 */
	body, baseModel := thinking.ApplyThinking(requestBody, model)

	/* 转换请求格式 */
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, false)
	codexBody, _ = sjson.SetBytes(codexBody, "model", baseModel)
	codexBody, _ = sjson.DeleteBytes(codexBody, "previous_response_id")
	codexBody, _ = sjson.DeleteBytes(codexBody, "prompt_cache_retention")
	codexBody, _ = sjson.DeleteBytes(codexBody, "safety_identifier")
	codexBody, _ = sjson.DeleteBytes(codexBody, "generate")
	if !gjson.GetBytes(codexBody, "instructions").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "instructions", "")
	}

	/* 构建并发送请求 - 使用 /responses/compact 端点 */
	apiURL := e.baseURL + "/responses/compact"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(codexBody))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	applyCodexHeaders(httpReq, account, false)

	httpResp, err := e.httpClient.Do(httpReq)
	if err != nil {
		account.RecordFailure()
		return nil, fmt.Errorf("请求发送失败: %w", err)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(httpResp.Body)
		handleAccountError(account, httpResp.StatusCode, errBody)
		return nil, &StatusError{Code: httpResp.StatusCode, Body: errBody}
	}

	/* 读取完整响应并直接返回（compact 格式透传） */
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	return data, nil
}

/**
 * RawResponse 原始上游响应封装
 * @field StatusCode - HTTP 状态码
 * @field Body - 响应体（调用方负责关闭）
 * @field ErrBody - 错误时的响应体（StatusCode >= 300 时有值）
 */
type RawResponse struct {
	StatusCode int
	Body       io.ReadCloser
	ErrBody    []byte
}

/**
 * ExecuteRawCodexStream 发送请求到 Codex 并返回原始上游响应
 * 不做任何格式转换，由调用方自行处理响应体
 * 用于 Claude API 等需要自定义响应格式的场景
 *
 * @param ctx - 上下文
 * @param account - 使用的账号
 * @param requestBody - OpenAI Chat Completions 格式的请求体
 * @param model - 模型名称（可能含思考后缀）
 * @returns *RawResponse - 原始响应（成功时调用方需关闭 Body）
 * @returns error - 请求发送失败时返回错误
 */
func (e *Executor) ExecuteRawCodexStream(ctx context.Context, account *auth.Account, requestBody []byte, model string) (*RawResponse, error) {
	/* 应用思考配置 */
	body, baseModel := thinking.ApplyThinking(requestBody, model)

	/* 转换请求格式 */
	codexBody := translator.ConvertOpenAIRequestToCodex(baseModel, body, true)
	codexBody, _ = sjson.SetBytes(codexBody, "model", baseModel)
	codexBody, _ = sjson.SetBytes(codexBody, "stream", true)
	codexBody, _ = sjson.DeleteBytes(codexBody, "previous_response_id")
	codexBody, _ = sjson.DeleteBytes(codexBody, "prompt_cache_retention")
	codexBody, _ = sjson.DeleteBytes(codexBody, "safety_identifier")
	codexBody, _ = sjson.DeleteBytes(codexBody, "generate")
	if !gjson.GetBytes(codexBody, "instructions").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "instructions", "")
	}

	/* 构建并发送请求 */
	apiURL := e.baseURL + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(codexBody))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	applyCodexHeaders(httpReq, account, true)

	httpResp, err := e.httpClient.Do(httpReq)
	if err != nil {
		account.RecordFailure()
		return nil, fmt.Errorf("请求发送失败: %w", err)
	}

	/* 错误状态码：读取错误体并返回 */
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(httpResp.Body)
		_ = httpResp.Body.Close()
		handleAccountError(account, httpResp.StatusCode, errBody)
		return &RawResponse{StatusCode: httpResp.StatusCode, ErrBody: errBody}, &StatusError{Code: httpResp.StatusCode, Body: errBody}
	}

	/* 成功：返回原始响应体，调用方负责关闭 */
	return &RawResponse{StatusCode: httpResp.StatusCode, Body: httpResp.Body}, nil
}

/**
 * applyCodexHeaders 设置 Codex API 请求头
 * @param r - HTTP 请求
 * @param account - 账号（提供 access_token 和 account_id）
 * @param stream - 是否为流式请求
 */
func applyCodexHeaders(r *http.Request, account *auth.Account, stream bool) {
	token := account.GetAccessToken()
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Version", codexClientVersion)
	r.Header.Set("Session_id", uuid.NewString())
	r.Header.Set("User-Agent", codexUserAgent)
	r.Header.Set("Originator", "codex_cli_rs")
	r.Header.Set("Connection", "Keep-Alive")

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}

	accountID := account.GetAccountID()
	if accountID != "" {
		r.Header.Set("Chatgpt-Account-Id", accountID)
	}
}

/**
 * handleAccountError 根据 HTTP 错误状态码记录账号失败
 * handler 层会根据 ShouldRemoveAccount 决定是否删号
 * @param account - 账号
 * @param statusCode - HTTP 状态码
 * @param body - 错误响应体
 */
func handleAccountError(account *auth.Account, statusCode int, body []byte) {
	account.RecordFailure()

	switch {
	case statusCode == 429:
		/* 配额耗尽，设置配额冷却 */
		cooldown := parseRetryAfter(body)
		if cooldown > 0 {
			account.SetQuotaCooldown(cooldown)
		}
	case statusCode == 403:
		account.SetCooldown(5 * time.Minute)
	}

	log.Warnf("账号 [%s] 请求失败 [%d]: %s", account.GetEmail(), statusCode, summarizeError(body))
}

/**
 * ShouldRemoveAccount 判断此错误是否应该导致账号被立即删除（内存+磁盘）
 * 401（认证失效）现在由 Manager.HandleAuth401 异步处理（先冷却+后台刷新），不在此处直接删除
 * 403（地域封锁/Cloudflare 拦截）、400（参数错误）、429（限频）、5xx（服务端）均不删号
 * @param statusCode - HTTP 状态码
 * @returns bool - 是否应该立即删除
 */
func ShouldRemoveAccount(statusCode int) bool {
	/* 401 由 HandleAuth401 异步处理，此处不再直接删除 */
	return false
}

/**
 * StatusError HTTP 状态错误
 * @field Code - HTTP 状态码
 * @field Body - 错误响应体
 */
type StatusError struct {
	Code int
	Body []byte
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("Codex API 错误 [%d]: %s", e.Code, summarizeError(e.Body))
}

/**
 * summarizeError 提取错误响应的摘要信息
 * @param body - 错误响应体
 * @returns string - 错误摘要
 */
func summarizeError(body []byte) string {
	if len(body) == 0 {
		return "(空响应)"
	}
	if msg := gjson.GetBytes(body, "error.message").String(); msg != "" {
		return msg
	}
	if len(body) > 200 {
		return string(body[:200]) + "..."
	}
	return string(body)
}

/**
 * parseRetryAfter 从 429 错误响应中解析冷却时间
 * @param body - 错误响应体
 * @returns time.Duration - 冷却持续时间
 */
func parseRetryAfter(body []byte) time.Duration {
	if len(body) == 0 {
		return 60 * time.Second
	}

	/* 尝试从 resets_at 字段解析 */
	if resetsAt := gjson.GetBytes(body, "error.resets_at").Int(); resetsAt > 0 {
		resetTime := time.Unix(resetsAt, 0)
		if resetTime.After(time.Now()) {
			return time.Until(resetTime)
		}
	}

	/* 尝试从 resets_in_seconds 字段解析 */
	if seconds := gjson.GetBytes(body, "error.resets_in_seconds").Int(); seconds > 0 {
		return time.Duration(seconds) * time.Second
	}

	/* 默认冷却 60 秒 */
	return 60 * time.Second
}

/**
 * extractUsageFromSSE 从 SSE 数据中提取 response.completed 事件的 usage 信息
 * 并记录到账号的 token 使用量统计
 * @param data - 完整的 SSE 响应数据
 * @param account - 要记录 usage 的账号
 */
func extractUsageFromSSE(data []byte, account *auth.Account) {
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		jsonData := bytes.TrimSpace(line[5:])
		if gjson.GetBytes(jsonData, "type").String() != "response.completed" {
			continue
		}
		usage := gjson.GetBytes(jsonData, "response.usage")
		if !usage.Exists() {
			continue
		}
		inputTokens := usage.Get("input_tokens").Int()
		outputTokens := usage.Get("output_tokens").Int()
		totalTokens := usage.Get("total_tokens").Int()
		account.RecordUsage(inputTokens, outputTokens, totalTokens)
		return
	}
}

/**
 * extractUsageFromStreamLine 从单行 SSE 数据中提取 usage（用于流式场景）
 * @param line - 单行 SSE 数据
 * @param account - 要记录 usage 的账号
 */
func extractUsageFromStreamLine(line []byte, account *auth.Account) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	jsonData := bytes.TrimSpace(line[5:])
	if gjson.GetBytes(jsonData, "type").String() != "response.completed" {
		return
	}
	usage := gjson.GetBytes(jsonData, "response.usage")
	if !usage.Exists() {
		return
	}
	inputTokens := usage.Get("input_tokens").Int()
	outputTokens := usage.Get("output_tokens").Int()
	totalTokens := usage.Get("total_tokens").Int()
	account.RecordUsage(inputTokens, outputTokens, totalTokens)
}

/**
 * StartKeepAlive 启动连接池保活循环
 * 每隔固定时间向上游发送轻量级 HEAD 请求，防止空闲连接被回收
 * 解决长时间无请求后首次请求因重建 TCP+TLS 连接而耗时过长的问题
 * 使用 sync.Once 保证只启动一次
 * @param ctx - 上下文，用于控制生命周期
 */
func (e *Executor) StartKeepAlive(ctx context.Context) {
	e.keepAliveOnce.Do(func() {
		go func() {
			/* 每 60 秒 ping 一次上游，保持连接池热度 */
			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()

			pingURL := strings.TrimSuffix(e.baseURL, "/codex")
			if pingURL == "" {
				pingURL = "https://chatgpt.com"
			}

			log.Infof("连接保活已启动，每 60 秒 ping %s", pingURL)

			for {
				select {
				case <-ctx.Done():
					log.Debug("连接保活循环已停止")
					return
				case <-ticker.C:
					e.pingUpstream(pingURL)
				}
			}
		}()
	})
}

/**
 * pingUpstream 向上游发送轻量级 HEAD 请求保持连接池活跃
 * 忽略响应结果，仅为维持 TCP+TLS 连接
 * @param targetURL - 目标 URL
 */
func (e *Executor) pingUpstream(targetURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, targetURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", codexUserAgent)
	req.Header.Set("Connection", "Keep-Alive")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		log.Debugf("连接保活 ping 失败: %v", err)
		return
	}
	_ = resp.Body.Close()
	log.Debugf("连接保活 ping 成功: %d", resp.StatusCode)
}
