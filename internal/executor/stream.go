package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"codex-proxy/internal/auth"
	"codex-proxy/internal/thinking"
	"codex-proxy/internal/translator"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CodexResponsesStream 上游 /responses 已返回 2xx 后的可读流（由 Pump* 负责关闭 Body）。
type CodexResponsesStream struct {
	body         io.ReadCloser
	account      *auth.Account
	Attempts     int
	BaseModel    string
	ConvertDur   time.Duration
	SendDur      time.Duration
	reverseTools map[string]string
	/* IncludeUsage 为 true 时按 OpenAI stream_options.include_usage 在 [DONE] 前追加 choices 为 [] 的 usage 块 */
	IncludeUsage bool
}

// CodexResponsesMeta bundles metadata returned by openCodexResponsesBody.
type CodexResponsesMeta struct {
	Account      *auth.Account
	Attempts     int
	BaseModel    string
	ConvertDur   time.Duration
	SendDur      time.Duration
	ReverseTools map[string]string
}

/* prefixThenRestCloser 首读已拉取的字节 + 剩余 Body，供在返回给客户端前探测 GOAWAY 后仍能透传已读数据 */
type prefixThenRestCloser struct {
	prefix []byte
	off    int
	rest   io.ReadCloser
}

func (p *prefixThenRestCloser) Read(b []byte) (int, error) {
	if p.off < len(p.prefix) {
		n := copy(b, p.prefix[p.off:])
		p.off += n
		return n, nil
	}
	if p.rest == nil {
		return 0, io.EOF
	}
	return p.rest.Read(b)
}

func (p *prefixThenRestCloser) Close() error {
	if p.rest == nil {
		return nil
	}
	err := p.rest.Close()
	p.rest = nil
	return err
}

// openCodexResponsesBody 与 OpenCodexResponsesStream 相同：选号、sendWithRetry、首读探测空体/可重试读错并换号。
// Claude 原始流等非 Pump 路径也经此打开，避免 200 + 空 body 导致客户端 SSE 体完全无字节。
func (e *Executor) openCodexResponsesBody(ctx context.Context, rc RetryConfig, requestBody []byte, model string) (bodyRC io.ReadCloser, meta CodexResponsesMeta, err error) {
	convertStart := time.Now()
	thBody, bm := thinking.ApplyThinking(requestBody, model)
	meta.BaseModel = bm
	codexBody := translator.ConvertOpenAIRequestToCodex(meta.BaseModel, thBody, true)
	meta.ConvertDur = time.Since(convertStart)
	apiURL := e.baseURL + "/responses"
	sendStart := time.Now()

	readRounds := 1 + rc.EmptyRetryMax
	if readRounds < 2 {
		readRounds = 2
	}
	if rc.EmptyRetryMax < 0 {
		readRounds = 2
	}
	excluded := make(map[string]bool)

	for round := 0; round < readRounds; round++ {
		if ctx.Err() != nil {
			meta.SendDur = time.Since(sendStart)
			return nil, meta, ctx.Err()
		}
		rcExcl := MergeRetryConfigExcluded(rc, excluded)
		httpResp, acc, att, serr := e.sendWithRetry(ctx, rcExcl, model, apiURL, codexBody, true)
		if serr != nil {
			meta.SendDur = time.Since(sendStart)
			return nil, meta, serr
		}

		buf := make([]byte, 32768)
		n, rerr := httpResp.Body.Read(buf)
		if rerr != nil && rerr != io.EOF {
			_ = httpResp.Body.Close()
			acc.RecordFailure()
			if isRetryableUpstreamReadErr(rerr) && round+1 < readRounds {
				excluded[acc.FilePath] = true
				log.Warnf("responses-stream 首读失败，换号/重建连接重试 (%d/%d) account=%s: %v", round+1, readRounds, acc.GetEmail(), wrapReadErr(rerr))
				continue
			}
			meta.SendDur = time.Since(sendStart)
			return nil, meta, fmt.Errorf("读取上游流失败: %w", wrapReadErr(rerr))
		}

		meta.SendDur = time.Since(sendStart)
		var br io.ReadCloser = httpResp.Body
		if n > 0 {
			prefix := append([]byte(nil), buf[:n]...)
			br = &prefixThenRestCloser{prefix: prefix, rest: httpResp.Body}
		} else if rerr == io.EOF {
			_ = httpResp.Body.Close()
			acc.RecordFailure()
			if round+1 < readRounds {
				excluded[acc.FilePath] = true
				log.Warnf("responses-stream 上游立即 EOF，换号重试 (%d/%d) account=%s", round+1, readRounds, acc.GetEmail())
				continue
			}
			return nil, meta, fmt.Errorf("读取上游流失败: 空响应")
		}

		meta.Account = acc
		meta.Attempts = att
		meta.ReverseTools = translator.BuildReverseToolNameMap(requestBody)
		return br, meta, nil
	}
	meta.SendDur = time.Since(sendStart)
	return nil, meta, fmt.Errorf("读取上游流失败")
}

// OpenCodexResponsesStream 完成选号、重试与首包前的 HTTP 往返；调用方在写入客户端 SSE 头后再 Pump。
// 在返回前做一次首读：若立即遇 GOAWAY 等可重试错误则关连接换号重来，减少「已 200 后 pump 才断」的失败率。
func (e *Executor) OpenCodexResponsesStream(ctx context.Context, rc RetryConfig, requestBody []byte, model string) (*CodexResponsesStream, error) {
	bodyRC, meta, err := e.openCodexResponsesBody(ctx, rc, requestBody, model)
	if err != nil {
		return nil, err
	}
	includeUsage := gjson.GetBytes(requestBody, "stream_options.include_usage").Bool()
	return &CodexResponsesStream{
		body:         bodyRC,
		account:      meta.Account,
		Attempts:     meta.Attempts,
		BaseModel:    meta.BaseModel,
		ConvertDur:   meta.ConvertDur,
		SendDur:      meta.SendDur,
		reverseTools: meta.ReverseTools,
		IncludeUsage: includeUsage,
	}, nil
}

// PumpChatCompletion 将 Codex SSE 转为 OpenAI Chat Completions 块写入 w（客户端 SSE 头须已由 handler 写好）。
func (s *CodexResponsesStream) PumpChatCompletion(w io.Writer, flush func()) error {
	defer func() { _ = s.body.Close() }()

	reverseToolMap := s.reverseTools
	state := translator.NewStreamState(s.BaseModel)
	scanner := bufio.NewScanner(s.body)
	scanner.Buffer(make([]byte, scannerInitSize), scannerMaxSize)
	streamStart := time.Now()
	var firstChunkAt time.Time
	var completedAt time.Time
	chunkCount := 0
	pumpCtx := context.Background()

	for scanner.Scan() {
		line := scanner.Bytes()
		chunks := translator.ConvertStreamChunk(pumpCtx, line, state, reverseToolMap, s.IncludeUsage)
		for _, chunk := range chunks {
			if firstChunkAt.IsZero() {
				firstChunkAt = time.Now()
			}
			chunkCount++
			_, _ = w.Write(sseDataPrefix)
			_, _ = io.WriteString(w, chunk)
			_, _ = w.Write(sseDataSuffix)
			if flush != nil {
				flush()
			}
		}
		if state.Completed {
			if completedAt.IsZero() {
				completedAt = time.Now()
			}
			break
		}
	}

	if err := scanner.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			firstChunkDur := time.Duration(0)
			if !firstChunkAt.IsZero() {
				firstChunkDur = firstChunkAt.Sub(streamStart)
			}
			log.Infof("req summary stream model=%s account=%s attempts=%d convert=%v upstream_ttfb=%v first_chunk=%v to_completed=%v tail_after_completed=%v stream=%v chunks=%d total=%v (canceled)", s.BaseModel, s.account.GetEmail(), s.Attempts, s.ConvertDur, s.SendDur, firstChunkDur, time.Duration(0), time.Duration(0), time.Since(streamStart), chunkCount, time.Since(streamStart))
			/* 已向客户端承诺 SSE：无任何 data 时须返回错误，避免 200 + 空体 */
			if chunkCount == 0 {
				return fmt.Errorf("读取流式响应中断: %w", err)
			}
			return nil
		}
		log.Errorf("读取流式响应失败: %v", err)
		firstChunkDur := time.Duration(0)
		completedDur := time.Duration(0)
		tailAfterCompleted := time.Duration(0)
		if !firstChunkAt.IsZero() {
			firstChunkDur = firstChunkAt.Sub(streamStart)
		}
		if !completedAt.IsZero() {
			completedDur = completedAt.Sub(streamStart)
			tailAfterCompleted = time.Since(completedAt)
		}
		log.Infof("req summary stream model=%s account=%s attempts=%d convert=%v upstream_ttfb=%v first_chunk=%v to_completed=%v tail_after_completed=%v stream=%v chunks=%d total=%v (ERR)", s.BaseModel, s.account.GetEmail(), s.Attempts, s.ConvertDur, s.SendDur, firstChunkDur, completedDur, tailAfterCompleted, time.Since(streamStart), chunkCount, time.Since(streamStart))
		return wrapReadErr(err)
	}
	/* response.completed 会写出带 finish_reason 的 chunk，但未必有正文；仍视为有效流并须写 [DONE] */
	if !state.HasText && !state.HasToolCall && !state.HasReasoning && !state.Completed {
		firstChunkDur := time.Duration(0)
		completedDur := time.Duration(0)
		tailAfterCompleted := time.Duration(0)
		if !firstChunkAt.IsZero() {
			firstChunkDur = firstChunkAt.Sub(streamStart)
		}
		if !completedAt.IsZero() {
			completedDur = completedAt.Sub(streamStart)
			tailAfterCompleted = time.Since(completedAt)
		}
		log.Infof("req summary stream model=%s account=%s attempts=%d convert=%v upstream_ttfb=%v first_chunk=%v to_completed=%v tail_after_completed=%v stream=%v chunks=%d total=%v (empty)", s.BaseModel, s.account.GetEmail(), s.Attempts, s.ConvertDur, s.SendDur, firstChunkDur, completedDur, tailAfterCompleted, time.Since(streamStart), chunkCount, time.Since(streamStart))
		return ErrEmptyResponse
	}

	if !state.Completed {
		finishReason := "stop"
		if state.FunctionCallIndex != -1 {
			finishReason = "tool_calls"
		}
		synth := `{"id":"","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
		synth, _ = sjson.Set(synth, "id", state.ResponseID)
		synth, _ = sjson.Set(synth, "created", state.CreatedAt)
		synth, _ = sjson.Set(synth, "model", state.Model)
		synth, _ = sjson.Set(synth, "choices.0.finish_reason", finishReason)
		chunkCount++
		_, _ = w.Write(sseDataPrefix)
		_, _ = io.WriteString(w, synth)
		_, _ = w.Write(sseDataSuffix)
		if flush != nil {
			flush()
		}
	}

	if s.IncludeUsage {
		usageChunk := translator.BuildChatCompletionStreamUsageOnlyChunk(state)
		chunkCount++
		_, _ = w.Write(sseDataPrefix)
		_, _ = io.WriteString(w, usageChunk)
		_, _ = w.Write(sseDataSuffix)
		if flush != nil {
			flush()
		}
	}

	doneWriteStart := time.Now()
	_, _ = w.Write(sseDoneMarker)
	if flush != nil {
		flush()
	}
	doneWriteDur := time.Since(doneWriteStart)

	if state.UsageInput > 0 || state.UsageOutput > 0 {
		s.account.RecordUsage(state.UsageInput, state.UsageOutput, state.UsageTotal)
	}
	s.account.RecordSuccess()
	firstChunkDur := time.Duration(0)
	completedDur := time.Duration(0)
	tailAfterCompleted := time.Duration(0)
	if !firstChunkAt.IsZero() {
		firstChunkDur = firstChunkAt.Sub(streamStart)
	}
	if !completedAt.IsZero() {
		completedDur = completedAt.Sub(streamStart)
		tailAfterCompleted = time.Since(completedAt)
	}
	log.Infof("req summary stream model=%s account=%s attempts=%d convert=%v upstream_ttfb=%v first_chunk=%v to_completed=%v tail_after_completed=%v done_write=%v stream=%v chunks=%d total=%v", s.BaseModel, s.account.GetEmail(), s.Attempts, s.ConvertDur, s.SendDur, firstChunkDur, completedDur, tailAfterCompleted, doneWriteDur, time.Since(streamStart), chunkCount, time.Since(streamStart))
	return nil
}

// PumpRawSSE 原样转发上游 SSE 字节（Responses API）。
func (s *CodexResponsesStream) PumpRawSSE(w io.Writer, flush func()) error {
	defer func() { _ = s.body.Close() }()
	buf := make([]byte, httpBufferSize)
	streamStart := time.Now()
	for {
		n, readErr := s.body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flush != nil {
				flush()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				s.account.RecordSuccess()
				log.Infof("req summary responses-stream model=%s account=%s attempts=%d convert=%v upstream=%v total=%v", s.BaseModel, s.account.GetEmail(), s.Attempts, s.ConvertDur, s.SendDur, time.Since(streamStart))
				return nil
			}
			if errors.Is(readErr, context.Canceled) {
				log.Infof("req summary responses-stream model=%s account=%s attempts=%d convert=%v upstream=%v total=%v (canceled)", s.BaseModel, s.account.GetEmail(), s.Attempts, s.ConvertDur, s.SendDur, time.Since(streamStart))
				return nil
			}
			log.Errorf("读取流式响应失败: %v", readErr)
			log.Infof("req summary responses-stream model=%s account=%s attempts=%d convert=%v upstream=%v total=%v (ERR)", s.BaseModel, s.account.GetEmail(), s.Attempts, s.ConvertDur, s.SendDur, time.Since(streamStart))
			return wrapReadErr(readErr)
		}
	}
}

// CodexCompactStream /responses/compact 成功后的响应（含待透传头与 Body）。
type CodexCompactStream struct {
	Resp       *http.Response
	Account    *auth.Account
	Attempts   int
	BaseModel  string
	ConvertDur time.Duration
	SendDur    time.Duration
}

// PumpBody 透传 compact 响应体；成功读完时由调用方 RecordSuccess。
func (s *CodexCompactStream) PumpBody(w io.Writer, flush func()) error {
	defer func() { _ = s.Resp.Body.Close() }()
	buf := make([]byte, httpBufferSize)
	for {
		n, err := s.Resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flush != nil {
				flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return wrapReadErr(err)
		}
	}
}
