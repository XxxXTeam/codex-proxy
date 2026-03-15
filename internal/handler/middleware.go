/**
 * Gin 中间件：CORS 与预检处理
 */
package handler

import (
	"compress/gzip"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

/**
 * OptionsBypass 直接放行 OPTIONS 预检请求，避免触发鉴权或业务逻辑
 * @returns gin.HandlerFunc - Gin 中间件函数
 */
func OptionsBypass() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodOptions {
			c.Next()
			return
		}

		origin := c.GetHeader("Origin")
		if origin == "" {
			origin = "*"
		}
		allowMethods := c.GetHeader("Access-Control-Request-Method")
		if allowMethods == "" {
			allowMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
		}
		allowHeaders := c.GetHeader("Access-Control-Request-Headers")
		if allowHeaders == "" {
			allowHeaders = "Authorization, Content-Type"
		}

		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Vary", "Origin")
		c.Header("Access-Control-Allow-Methods", allowMethods)
		c.Header("Access-Control-Allow-Headers", allowHeaders)
		c.Header("Access-Control-Max-Age", "86400")
		c.Status(http.StatusNoContent)
		c.Abort()
	}
}

/**
 * CORSAllowOrigin 确保所有响应都包含 Access-Control-Allow-Origin
 * @returns gin.HandlerFunc - Gin 中间件函数
 */
func CORSAllowOrigin() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			origin = "*"
		}
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Vary", "Origin")
		c.Next()
	}
}

/**
 * GzipIfAccepted 当客户端声明支持 gzip 时，启用响应压缩
 * @returns gin.HandlerFunc - Gin 中间件函数
 */
func GzipIfAccepted() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		if isV1Path(c.Request.URL.Path) {
			c.Next()
			return
		}
		if !clientAcceptsGzip(c.Request) {
			c.Next()
			return
		}
		if c.Writer.Header().Get("Content-Encoding") != "" {
			c.Next()
			return
		}

		c.Header("Content-Encoding", "gzip")
		c.Header("Vary", "Accept-Encoding")

		gz := gzip.NewWriter(c.Writer)
		defer func() {
			_ = gz.Close()
		}()

		c.Writer = &gzipWriter{ResponseWriter: c.Writer, Writer: gz}
		c.Next()
	}
}

type gzipWriter struct {
	gin.ResponseWriter
	Writer *gzip.Writer
}

func (w *gzipWriter) Write(data []byte) (int, error) {
	return w.Writer.Write(data)
}

func clientAcceptsGzip(r *http.Request) bool {
	enc := r.Header.Get("Accept-Encoding")
	return enc != "" && strings.Contains(enc, "gzip")
}

func isV1Path(path string) bool {
	return path == "/v1" || strings.HasPrefix(path, "/v1/")
}

func (w *gzipWriter) WriteHeader(statusCode int) {
	ctype := w.Header().Get("Content-Type")
	if strings.HasPrefix(strings.ToLower(ctype), "text/event-stream") {
		w.Header().Del("Content-Encoding")
		w.ResponseWriter.WriteHeader(statusCode)
		return
	}
	w.ResponseWriter.WriteHeader(statusCode)
}
