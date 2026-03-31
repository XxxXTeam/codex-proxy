package netutil

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

// WrapDialWithTCPNoDelay 包装 DialContext，在成功拨号后对底层 TCP 设置 SetNoDelay(true)，
// 减轻 Nagle 与批量 ACK 对交互式/SSE 首包延迟的影响。
func WrapDialWithTCPNoDelay(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	if dial == nil {
		return nil
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		setTCPNoDelayChain(c)
		return c, nil
	}
}

func setTCPNoDelayChain(c net.Conn) {
	for c != nil {
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			return
		}
		type netConnHolder interface{ NetConn() net.Conn }
		if h, ok := c.(netConnHolder); ok {
			c = h.NetConn()
			continue
		}
		return
	}
}

// NormalizeResolveAddress normalizes resolve target input and supports:
// - host
// - host:port
// - https://host[:port][/path]
// - host[/path]
// It also removes trailing dot from FQDN.
func NormalizeResolveAddress(input string) string {
	v := strings.TrimSpace(input)
	if v == "" {
		return ""
	}

	if strings.Contains(v, "://") {
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			v = u.Host
		}
	}

	if i := strings.Index(v, "/"); i >= 0 {
		v = v[:i]
	}
	v = strings.TrimSpace(v)
	v = strings.TrimSuffix(v, ".")
	return v
}

func normalizeTargetHost(targetHost string) string {
	return strings.TrimSuffix(strings.TrimSpace(strings.ToLower(targetHost)), ".")
}

func rewriteTargetAddress(targetHost, resolveAddress, addr string) string {
	if targetHost == "" || resolveAddress == "" {
		return addr
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if !strings.EqualFold(host, targetHost) {
		return addr
	}

	if _, _, splitErr := net.SplitHostPort(resolveAddress); splitErr == nil {
		return resolveAddress
	}
	return net.JoinHostPort(resolveAddress, port)
}

// ParseProxyScheme returns the normalized proxy scheme.
func ParseProxyScheme(proxyURL string) (string, error) {
	parsedURL, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil {
		return "", err
	}
	return strings.ToLower(parsedURL.Scheme), nil
}

// BuildResolveDialContext returns a DialContext that redirects connections for targetHost
// to resolveAddress (host or host:port). If resolveAddress is empty, it returns dialer.DialContext.
func BuildResolveDialContext(dialer *net.Dialer, targetHost, resolveAddress string) func(context.Context, string, string) (net.Conn, error) {
	targetHost = normalizeTargetHost(targetHost)
	resolveAddress = NormalizeResolveAddress(resolveAddress)
	if targetHost == "" || resolveAddress == "" {
		return dialer.DialContext
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, rewriteTargetAddress(targetHost, resolveAddress, addr))
	}
}

// BuildUpstreamDialContext creates a DialContext with consistent resolve/proxy behavior.
// HTTP/HTTPS proxies are handled by http.Transport#Proxy, SOCKS5 proxies by a custom dialer.
func BuildUpstreamDialContext(dialer *net.Dialer, proxyURL, targetHost, resolveAddress string) func(context.Context, string, string) (net.Conn, error) {
	baseDialer := BuildResolveDialContext(dialer, targetHost, resolveAddress)
	if strings.TrimSpace(proxyURL) == "" {
		return baseDialer
	}

	scheme, err := ParseProxyScheme(proxyURL)
	if err != nil {
		log.Warnf("代理 URL 解析失败: %v，将忽略代理配置", err)
		return baseDialer
	}

	if scheme == "socks5" || scheme == "socks5h" {
		return BuildProxyDialContext(dialer, proxyURL, targetHost, resolveAddress)
	}
	return baseDialer
}

// BuildProxyDialContext 支持 HTTP/HTTPS/SOCKS5 代理
// 结合 DNS 解析和代理功能
// proxyURL 支持: http://host:port, https://host:port, socks5://host:port
// 支持代理认证: socks5://user:pass@host:port
func BuildProxyDialContext(dialer *net.Dialer, proxyURL, targetHost, resolveAddress string) func(context.Context, string, string) (net.Conn, error) {
	targetHost = normalizeTargetHost(targetHost)
	resolveAddress = NormalizeResolveAddress(resolveAddress)
	baseDialer := BuildResolveDialContext(dialer, targetHost, resolveAddress)

	if proxyURL == "" {
		return baseDialer
	}

	parsedURL, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil {
		log.Warnf("代理 URL 解析失败: %v，将忽略代理配置", err)
		return baseDialer
	}

	scheme := strings.ToLower(parsedURL.Scheme)

	// HTTP/HTTPS 代理使用标准库支持，不需要特殊处理这里
	// 在 http.Transport 层面处理（通过 transport.Proxy）

	// SOCKS5 代理需要自定义 DialContext
	if scheme == "socks5" || scheme == "socks5h" {
		socksDialer, err := buildSOCKS5Dialer(dialer, parsedURL)
		if err != nil {
			log.Warnf("SOCKS5 代理创建失败: %v，将使用直连", err)
			return baseDialer
		}
		log.Debugf("代理方案 '%s' 通过自定义 DialContext 处理", scheme)

		// 返回一个适配器函数，将 proxy.Dialer 适配为 DialContext
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			targetAddr := rewriteTargetAddress(targetHost, resolveAddress, addr)
			if scheme == "socks5" && resolveAddress == "" {
				targetAddr, err = resolveSOCKS5Target(ctx, dialer, targetAddr)
				if err != nil {
					return nil, err
				}
			}
			if contextDialer, ok := socksDialer.(proxy.ContextDialer); ok {
				return contextDialer.DialContext(ctx, network, targetAddr)
			}
			return socksDialer.Dial(network, targetAddr)
		}
	}

	log.Debugf("代理方案 '%s' 由 http.Transport#Proxy 处理", scheme)
	return baseDialer
}

// buildSOCKS5Dialer 创建 SOCKS5 代理拨号器
// 支持认证: socks5://user:pass@host:port
func buildSOCKS5Dialer(baseDialer *net.Dialer, proxyURL *url.URL) (proxy.Dialer, error) {
	var auth *proxy.Auth
	if proxyURL.User != nil {
		auth = &proxy.Auth{User: proxyURL.User.Username()}
		if password, ok := proxyURL.User.Password(); ok {
			auth.Password = password
		}
	}

	proxyDialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, baseDialer)
	if err != nil {
		return nil, fmt.Errorf("创建 SOCKS5 代理失败: %w", err)
	}

	return proxyDialer, nil
}

func resolveSOCKS5Target(ctx context.Context, dialer *net.Dialer, addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	if ip := net.ParseIP(host); ip != nil {
		return addr, nil
	}

	resolver := net.DefaultResolver
	if dialer != nil && dialer.Resolver != nil {
		resolver = dialer.Resolver
	}

	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", err
	}
	for _, ip := range ips {
		if v4 := ip.IP.To4(); v4 != nil {
			return net.JoinHostPort(v4.String(), port), nil
		}
	}
	for _, ip := range ips {
		if v6 := ip.IP.To16(); v6 != nil {
			return net.JoinHostPort(v6.String(), port), nil
		}
	}
	return "", fmt.Errorf("解析 SOCKS5 目标地址失败: %s", host)
}
