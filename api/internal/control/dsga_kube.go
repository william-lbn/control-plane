package control

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

// newDSGAKubeClients供凭据签发和创建Driver共用可信、有限期的typed API链。
// 只用中心自己的TLS/SA身份；不接受入站Agent Bearer、任意URL或跳过证书验证。
// tokenFile由标准rest传输按轮转读取；不跨网络持本地锁，不跟随redirect。
// 每组客户端共享限流器；这些默认值不是吞吐或规模认证。
func newDSGAKubeClients(kube *kubeClient) (kubernetes.Interface, vmclient.Interface, error) {
	if kube == nil || kube.http == nil {
		return nil, nil, errors.New("DSGA requires a trusted Kubernetes client")
	}
	endpoint, err := url.Parse(kube.base)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return nil, nil, errors.New("DSGA requires a fixed HTTPS Kubernetes API")
	}
	// 复用中心自身的已验证TLS传输与轮转SA token；入站Agent的Bearer只交给
	// TokenReview body，不能覆盖这里的Authorization。所有typed客户端共享
	// 同一有限速率器，避免每种API各有一份突发额度；数值不是规模/SLO认证。
	config := &rest.Config{Host: kube.base, Transport: kube.http.Transport, BearerToken: kube.token, BearerTokenFile: kube.tokenFile,
		Timeout: 5 * time.Second, RateLimiter: flowcontrol.NewTokenBucketRateLimiter(50, 100)}
	clientHTTP, err := rest.HTTPClientFor(config)
	if err != nil {
		return nil, nil, errors.New("DSGA Kubernetes transport unavailable")
	}
	clientHTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	clients, err := kubernetes.NewForConfigAndClient(config, clientHTTP)
	if err != nil {
		return nil, nil, errors.New("DSGA Kubernetes clients unavailable")
	}
	vms, err := vmclient.NewForConfigAndClient(config, clientHTTP)
	if err != nil {
		return nil, nil, errors.New("DSGA VM client unavailable")
	}
	return clients, vms, nil
}
