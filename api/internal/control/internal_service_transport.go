package control

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"time"

	"golang.org/x/net/proxy"
)

// Mac开发只改变内部Service的TCP通路；真实目标仍由原Service UID/ClusterIP
// 校验决定，原TLS CA/SNI、原生JWT及禁止redirect保持不变。这里不读取公共请求
// 的URL，也不使用全局HTTP_PROXY把凭据发往外网代理。生产未配置时仍直连。
func internalServiceSOCKS5() (string, error) {
	address := os.Getenv("NEON_INTERNAL_SOCKS5_ADDR")
	if address == "" {
		return "", nil
	}
	host, port, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	number, portErr := strconv.Atoi(port)
	if err != nil || ip == nil || !ip.IsLoopback() || portErr != nil || number < 1 || number > 65535 ||
		strconv.Itoa(number) != port {
		return "", errors.New("NEON_INTERNAL_SOCKS5_ADDR must be a literal loopback IP and TCP port")
	}
	return address, nil
}

func dialInternalService(ctx context.Context, network, address string) (net.Conn, error) {
	via, err := internalServiceSOCKS5()
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if via == "" {
		return dialer.DialContext(ctx, network, address)
	}
	if network != "tcp" {
		return nil, errors.New("internal Service tunnel only supports TCP")
	}
	tunnel, err := proxy.SOCKS5("tcp", via, nil, dialer)
	if err != nil {
		return nil, errors.New("internal Service tunnel unavailable")
	}
	contextDialer, ok := tunnel.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("internal Service tunnel must honor cancellation")
	}
	// 同一个有界拨号覆盖连接本地SSH以及SOCKS握手；失败不回落直连/其他代理。
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return contextDialer.DialContext(dialCtx, network, address)
}
