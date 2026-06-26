/**
 * 首页处理
 */
package handler

import (
	"io/fs"
	"mime"
	"path"
	"strings"

	"github.com/valyala/fasthttp"
)

/**
 * handleIndex 返回静态首页
 */
func (h *ProxyHandler) handleIndex(ctx *fasthttp.RequestCtx) {
	if h.staticAssets == nil {
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		return
	}
	indexHTML, err := fs.ReadFile(h.staticAssets, "assets/index.html")
	if err != nil {
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		return
	}
	ctx.SetContentType("text/html; charset=utf-8")
	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.SetBody(indexHTML)
}

func (h *ProxyHandler) handleStaticAsset(ctx *fasthttp.RequestCtx) {
	if h.staticAssets == nil {
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		return
	}

	rawPath := strings.TrimPrefix(string(ctx.Path()), "/assets/")
	cleanPath := strings.TrimPrefix(path.Clean("/"+rawPath), "/")
	if cleanPath == "." || cleanPath == "" {
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		return
	}

	body, err := fs.ReadFile(h.staticAssets, "assets/assets/"+cleanPath)
	if err != nil {
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		return
	}

	contentType := mime.TypeByExtension(path.Ext(cleanPath))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	ctx.SetContentType(contentType)
	ctx.Response.Header.Set("Cache-Control", "public, max-age=31536000, immutable")
	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.SetBody(body)
}
