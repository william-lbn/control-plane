package control

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"strings"
	"time"
)

// dsgaListenerConfig来自可信控制面进程配置。Agent的SA令牌不能发给使用Guest
// 内也可读取私钥的旧per-Compute TLS身份；新监听器的证书/私钥必须由控制面
// 独占，Agent只取得平台可信CA。这里不创建证书或部署资源，不复用Guest key。
type dsgaListenerConfig struct {
	Bind           string
	AgentNamespace string
	AgentAccount   string
	CertFile       string
	KeyFile        string
}

func dsgaServerCertificate(namespace, certFile, keyFile string) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	load := func() (*tls.Certificate, error) {
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil || len(pair.Certificate) == 0 {
			return nil, errors.New("DSGA dedicated TLS identity unavailable")
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil || leaf.IsCA || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
			return nil, errors.New("DSGA dedicated TLS certificate is not current")
		}
		wildcard, serverAuth := false, false
		for _, name := range leaf.DNSNames {
			wildcard = wildcard || name == "*."+namespace+".svc"
		}
		for _, usage := range leaf.ExtKeyUsage {
			serverAuth = serverAuth || usage == x509.ExtKeyUsageServerAuth
		}
		// 强制管理域wildcard，而不是现有Guest单Compute证书，避免误把已经
		// 分发给Guest的control.key作为SA令牌颁发服务身份。部署仍须保证该
		// 独立私钥只挂载到控制面；代码不能代替平台Secret/RBAC隔离验收。
		if !wildcard || !serverAuth {
			return nil, errors.New("DSGA requires a dedicated management-domain TLS certificate")
		}
		pair.Leaf = leaf
		return &pair, nil
	}
	if _, err := load(); err != nil {
		return nil, err
	}
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		suffix := "-management." + namespace + ".svc"
		if !strings.HasSuffix(hello.ServerName, suffix) || !computeWorkloadPattern.MatchString(strings.TrimSuffix(hello.ServerName, suffix)) {
			return nil, errors.New("DSGA exact Compute SNI required")
		}
		// 每次握手读取投影文件，支持平台原子换代；两文件短暂跨代/缺失时
		// 握手失败，不退回Guest密钥、旧身份或不验证TLS。现有连接遵守TLS
		// 建连语义，CA/连接生命周期与目标环境轮转仍需整链验收。
		pair, err := load()
		if err != nil || pair.Leaf.VerifyHostname(hello.ServerName) != nil {
			return nil, errors.New("DSGA dedicated TLS binding unavailable")
		}
		return pair, nil
	}, nil
}

func (gateway *computeGateway) dsgaServer(config dsgaListenerConfig) (*http.Server, error) {
	if config.AgentNamespace == "" && config.AgentAccount == "" && config.CertFile == "" && config.KeyFile == "" {
		return nil, nil
	}
	if gateway.kube == nil || config.Bind == "" || config.AgentNamespace == "" || config.AgentAccount == "" || config.CertFile == "" || config.KeyFile == "" {
		return nil, errors.New("complete dedicated DSGA TLS and subject configuration required")
	}
	certificate, err := dsgaServerCertificate(gateway.kube.namespace, config.CertFile, config.KeyFile)
	if err != nil {
		return nil, err
	}
	routes, err := gateway.routes(config.AgentNamespace, config.AgentAccount)
	if err != nil {
		return nil, err
	}
	return &http.Server{Addr: config.Bind, Handler: routes, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Second,
		// 执行代理独立总期限90秒，包含最多60秒PG等待；凭据路由仍有自己
		// 的10秒期限。不得复用旧15秒写期限截断已经执行的PG请求。
		WriteTimeout: 100 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certificate}}, nil
}
