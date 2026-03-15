/**
 * 首页处理
 */
package handler

import "github.com/gin-gonic/gin"

/**
 * handleIndex 返回静态首页
 */
func (h *ProxyHandler) handleIndex(c *gin.Context) {
	if len(h.indexHTML) == 0 {
		c.Status(404)
		return
	}
	c.Data(200, "text/html; charset=utf-8", h.indexHTML)
}
