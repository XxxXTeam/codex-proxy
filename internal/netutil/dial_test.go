package netutil

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestBuildProxyDialContextSOCKS5RewritesTargetHost(t *testing.T) {
	proxyAddr, targets, stop := startSOCKS5Recorder(t)
	defer stop()

	dialCtx := BuildProxyDialContext(
		&net.Dialer{Timeout: 3 * time.Second},
		"socks5://"+proxyAddr,
		"chatgpt.com",
		"1.2.3.4",
	)

	conn, err := dialCtx(context.Background(), "tcp", "chatgpt.com:443")
	if err != nil {
		t.Fatalf("dial through socks5: %v", err)
	}
	_ = conn.Close()

	select {
	case got := <-targets:
		if got != "1.2.3.4:443" {
			t.Fatalf("unexpected target via socks5 proxy: got %q want %q", got, "1.2.3.4:443")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for socks5 target")
	}
}

func TestBuildUpstreamDialContextHTTPProxyKeepsResolveRewrite(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() {
		_ = ln.Close()
	}()

	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()

	dialCtx := BuildUpstreamDialContext(
		&net.Dialer{Timeout: 3 * time.Second},
		"http://127.0.0.1:7890",
		"chatgpt.com",
		ln.Addr().String(),
	)

	conn, err := dialCtx(context.Background(), "tcp", "chatgpt.com:443")
	if err != nil {
		t.Fatalf("dial with resolve rewrite: %v", err)
	}
	_ = conn.Close()

	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for rewritten direct dial")
	}
}

func TestBuildProxyDialContextSOCKS5ResolvesHostnameLocally(t *testing.T) {
	proxyAddr, targets, stop := startSOCKS5Recorder(t)
	defer stop()

	dialCtx := BuildProxyDialContext(
		&net.Dialer{Timeout: 3 * time.Second},
		"socks5://"+proxyAddr,
		"",
		"",
	)

	conn, err := dialCtx(context.Background(), "tcp", "localhost:443")
	if err != nil {
		t.Fatalf("dial through socks5: %v", err)
	}
	_ = conn.Close()

	select {
	case got := <-targets:
		host, _, err := net.SplitHostPort(got)
		if err != nil {
			t.Fatalf("split target %q: %v", got, err)
		}
		if host == "localhost" {
			t.Fatalf("expected socks5 local resolution, got unresolved host %q", got)
		}
		if net.ParseIP(host) == nil {
			t.Fatalf("expected IP target after local resolution, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for socks5 target")
	}
}

func TestBuildProxyDialContextSOCKS5HPreservesHostname(t *testing.T) {
	proxyAddr, targets, stop := startSOCKS5Recorder(t)
	defer stop()

	dialCtx := BuildProxyDialContext(
		&net.Dialer{Timeout: 3 * time.Second},
		"socks5h://"+proxyAddr,
		"",
		"",
	)

	conn, err := dialCtx(context.Background(), "tcp", "example.invalid:443")
	if err != nil {
		t.Fatalf("dial through socks5h: %v", err)
	}
	_ = conn.Close()

	select {
	case got := <-targets:
		if got != "example.invalid:443" {
			t.Fatalf("unexpected target via socks5h proxy: got %q want %q", got, "example.invalid:443")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for socks5h target")
	}
}

func TestBuildProxyDialContextSOCKS5HonorsContextTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() {
		_ = ln.Close()
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() {
			_ = conn.Close()
		}()
		<-time.After(2 * time.Second)
	}()

	dialCtx := BuildProxyDialContext(
		&net.Dialer{Timeout: 3 * time.Second},
		"socks5://"+ln.Addr().String(),
		"",
		"",
	)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = dialCtx(ctx, "tcp", "127.0.0.1:443")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("dial did not honor context timeout, elapsed=%v err=%v", time.Since(start), err)
	}

	_ = ln.Close()
	<-done
}

func startSOCKS5Recorder(t *testing.T) (addr string, targets <-chan string, stop func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen socks5 recorder: %v", err)
	}

	targetCh := make(chan string, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() {
			_ = conn.Close()
		}()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		if err := handleSOCKS5Handshake(conn, targetCh); err != nil {
			t.Errorf("handle socks5 handshake: %v", err)
		}
	}()

	return ln.Addr().String(), targetCh, func() {
		_ = ln.Close()
		<-done
	}
}

func handleSOCKS5Handshake(conn net.Conn, targetCh chan<- string) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return err
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return err
	}

	reqHeader := make([]byte, 4)
	if _, err := io.ReadFull(conn, reqHeader); err != nil {
		return err
	}

	host, err := readSOCKS5Host(conn, reqHeader[3])
	if err != nil {
		return err
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return err
	}
	targetCh <- net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBuf))))

	_, err = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	return err
}

func readSOCKS5Host(conn net.Conn, atyp byte) (string, error) {
	switch atyp {
	case 0x01:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", err
		}
		return net.IP(buf).String(), nil
	case 0x03:
		size := make([]byte, 1)
		if _, err := io.ReadFull(conn, size); err != nil {
			return "", err
		}
		buf := make([]byte, int(size[0]))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", err
		}
		return string(buf), nil
	case 0x04:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", err
		}
		return net.IP(buf).String(), nil
	default:
		return "", io.ErrUnexpectedEOF
	}
}
