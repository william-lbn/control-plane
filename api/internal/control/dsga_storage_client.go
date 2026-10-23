package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

var errDSGAStorageManagement = errors.New("DSGA authenticated storage management is unavailable")

// 平台可信配置的引用，不含token/私钥/CA正文，不是公开创建请求中的目标地址。
// UID固定两个平台对象，Data允许平台按同UID轮转；每次客户端构造重验实际值。
// HTTPS只使用固定Service DNS及绑定CA，不能退回--dev、任意URL或跳过验签。
type dsgaStorageProfile struct {
	ProtocolVersion      uint32 `json:"protocol_version"`
	ServiceName          string `json:"service_name"`
	ServiceUID           string `json:"service_uid"`
	Port                 uint16 `json:"port"`
	CredentialSecretName string `json:"credential_secret_name"`
	CredentialSecretUID  string `json:"credential_secret_uid"`
}

func (p dsgaStorageProfile) validate(namespace string) error {
	if p.ProtocolVersion != 1 || p.Port == 0 || len(namespace) > 63 || !dsgaPlanNamespace.MatchString(namespace) ||
		len(validation.IsDNS1035Label(p.ServiceName)) != 0 || len(validation.IsDNS1123Subdomain(p.CredentialSecretName)) != 0 ||
		dsgapi.ValidateID(p.ServiceUID, "storage Service") != nil || dsgapi.ValidateID(p.CredentialSecretUID, "storage credential Secret") != nil {
		return errDSGAStorageManagement
	}
	return nil
}

type dsgaStorageClient struct {
	profile               dsgaStorageProfile
	namespace             string
	base                  string
	readToken, writeToken string
	publicKey             ed25519.PublicKey
	http                  *http.Client
}

// Kube SA仅用于读取平台Service/Secret，存储HTTP使用其自身正式JWT。两条身份
// 不互相替代，不把SA转发到controller。构造只快照复制值，不保留API对象指针；
// 无SQL锁/磁盘写入/密钥生成。平台引用不完整或验签失败时没有降级路径。
func newDSGAStorageClient(ctx context.Context, k *kubeClient, p dsgaStorageProfile) (*dsgaStorageClient, error) {
	if k == nil || p.validate(k.namespace) != nil {
		return nil, errDSGAStorageManagement
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	clients, _, err := newDSGAKubeClients(k)
	if err != nil {
		return nil, errDSGAStorageManagement
	}
	service, err := clients.CoreV1().Services(k.namespace).Get(ctx, p.ServiceName, metav1.GetOptions{})
	if err != nil {
		return nil, errDSGAStorageManagement
	}
	secret, err := clients.CoreV1().Secrets(k.namespace).Get(ctx, p.CredentialSecretName, metav1.GetOptions{})
	if err != nil {
		return nil, errDSGAStorageManagement
	}
	return dsgaStorageClientSnapshot(k.namespace, p, service, secret)
}

// 将两个只读API结果转成独立值和专用传输，便于分别验证平台读取与协议逻辑。
// 只有构造器使用当前可信Kube客户端获取快照；公开请求不能直接提交这些对象。
// UID固定来源，resourceVersion只证明真实读取，不冻结平台原地轮转token/证书。
func dsgaStorageClientSnapshot(namespace string, p dsgaStorageProfile, service *core.Service, secret *core.Secret) (*dsgaStorageClient, error) {
	if p.validate(namespace) != nil || service == nil || service.Namespace != namespace || service.Name != p.ServiceName || string(service.UID) != p.ServiceUID || service.ResourceVersion == "" || service.DeletionTimestamp != nil ||
		service.Spec.Type != core.ServiceTypeClusterIP || service.Spec.ExternalName != "" || len(service.Spec.ExternalIPs) != 0 || len(service.Spec.Ports) != 1 {
		return nil, errDSGAStorageManagement
	}
	ip := net.ParseIP(service.Spec.ClusterIP)
	port := service.Spec.Ports[0]
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || port.Protocol != core.ProtocolTCP || port.Port != int32(p.Port) || port.NodePort != 0 {
		return nil, errDSGAStorageManagement
	}
	if secret == nil || secret.Namespace != namespace || secret.Name != p.CredentialSecretName || string(secret.UID) != p.CredentialSecretUID || secret.ResourceVersion == "" || secret.DeletionTimestamp != nil || secret.Type != core.SecretTypeOpaque {
		return nil, errDSGAStorageManagement
	}
	readToken, writeToken := string(secret.Data["observe_token"]), string(secret.Data["create_token"])
	keyPEM, caPEM := secret.Data["public_key"], secret.Data["ca.crt"]
	if len(keyPEM) == 0 || len(keyPEM) > 4096 || len(caPEM) == 0 || len(caPEM) > 64<<10 {
		return nil, errDSGAStorageManagement
	}
	key, err := jwt.ParseEdPublicKeyFromPEM(keyPEM)
	public, ok := key.(ed25519.PublicKey)
	if err != nil || !ok || len(public) != ed25519.PublicKeySize || !validDSGAStorageToken(readToken, public, "scrubber") || !validDSGAStorageToken(writeToken, public, "pageserverapi") {
		return nil, errDSGAStorageManagement
	}
	ca := x509.NewCertPool()
	if !ca.AppendCertsFromPEM(bytes.Clone(caPEM)) {
		return nil, errDSGAStorageManagement
	}
	transport := &http.Transport{Proxy: nil, MaxConnsPerHost: 8, MaxIdleConns: 8, MaxIdleConnsPerHost: 8, IdleConnTimeout: 30 * time.Second,
		DisableCompression: true, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 64 << 10,
		TLSClientConfig: &tls.Config{RootCAs: ca, ServerName: p.ServiceName + "." + namespace + ".svc", MinVersion: tls.VersionTLS12}}
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &dsgaStorageClient{profile: p, namespace: namespace, base: "https://" + net.JoinHostPort(service.Spec.ClusterIP, strconv.Itoa(int(p.Port))), readToken: readToken, writeToken: writeToken, publicKey: bytes.Clone(public), http: client}, nil
}

// 对齐官方Scope::Scrubber/PageServerApi及其Admin兼容规则；不是自造scope。
// 原生token可无exp，若带exp/nbf则仍按标准时间校验。严格原始JSON拒绝重复键，
// 避免Go map与Rust Claims对同一签名有不同解释；任何错误都不回显token内容。
func validDSGAStorageToken(token string, key ed25519.PublicKey, required string) bool {
	if len(key) != ed25519.PublicKeySize || (required != "scrubber" && required != "pageserverapi") || len(token) == 0 || len(token) > 16<<10 || strings.ContainsAny(token, "\r\n\t ") {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	for i := 0; i < 2; i++ {
		raw, err := base64.RawURLEncoding.Strict().DecodeString(parts[i])
		if err != nil || dsgapi.ValidateJSON(raw, 16<<10, 256) != nil {
			return false
		}
	}
	parsed, err := jwt.Parse(token, func(*jwt.Token) (any, error) { return key, nil }, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithJSONNumber())
	if err != nil || parsed == nil || !parsed.Valid {
		return false
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return false
	}
	scope, ok := claims["scope"].(string)
	return ok && (scope == required || scope == "admin")
}

func (c *dsgaStorageClient) CloseIdleConnections() {
	if c != nil && c.http != nil {
		c.http.CloseIdleConnections()
	}
}

// 不保存或传播上游错误正文，防止远端把凭据/内部SQL反射进日志。保留有类型
// 状态码供原意图409/404恢复，超时/断开仍是未确认，不能写成“肯定没执行”。
type dsgaStorageHTTPError struct{ code int }

func (e dsgaStorageHTTPError) Error() string {
	return "DSGA storage management request was not confirmed"
}
func (e dsgaStorageHTTPError) Status() metav1.Status {
	reason := metav1.StatusReasonUnknown
	switch e.code {
	case 404:
		reason = metav1.StatusReasonNotFound
	case 409:
		reason = metav1.StatusReasonConflict
	case 429:
		reason = metav1.StatusReasonTooManyRequests
	case 401:
		reason = metav1.StatusReasonUnauthorized
	case 403:
		reason = metav1.StatusReasonForbidden
	}
	return metav1.Status{Status: metav1.StatusFailure, Reason: reason, Code: int32(e.code)}
}

// 只开放创建流程当前需要的四种官方路径，不暴露通用admin/DELETE请求器。
// URL目标来自固定Service，路径中的资源只接受规范32位ID，不允许query/转义。
func storageRequestScope(method, path string) (string, bool) {
	parts := strings.Split(path, "/")
	if method == http.MethodGet && len(parts) >= 4 && parts[0] == "control" && parts[1] == "v1" && parts[2] == "tenant" && dsgaStorageID.MatchString(parts[3]) {
		if len(parts) == 4 || (len(parts) == 6 && parts[4] == "timeline" && dsgaStorageID.MatchString(parts[5])) {
			return "scrubber", true
		}
	}
	if method == http.MethodPost && path == "v1/tenant" {
		return "pageserverapi", true
	}
	if method == http.MethodPost && len(parts) == 4 && parts[0] == "v1" && parts[1] == "tenant" && dsgaStorageID.MatchString(parts[2]) && parts[3] == "timeline" {
		return "pageserverapi", true
	}
	return "", false
}

func (c *dsgaStorageClient) request(ctx context.Context, method, path string, body any) (map[string]any, error) {
	scope, allowed := storageRequestScope(method, path)
	if c == nil || c.http == nil || !allowed || (method == http.MethodGet && body != nil) || (method == http.MethodPost && body == nil) {
		return nil, errDSGAStorageManagement
	}
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil || len(raw) > 16<<10 {
			return nil, errDSGAStorageManagement
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/"+path, bytes.NewReader(raw))
	if err != nil {
		return nil, errDSGAStorageManagement
	}
	token := c.readToken
	if scope == "pageserverapi" {
		token = c.writeToken
		req.Header.Set("Content-Type", "application/json")
	}
	// 构造时验签不代替发送时检查：客户端存活期间可跨过可选exp/nbf边界。
	// 失败只关闭本次调用；由下一轮原意图读取同UID Secret的新token，不在这里
	// 生成凭据、改计划或用另一scope重试。公钥为独立快照，调用间不修改。
	if !validDSGAStorageToken(token, c.publicKey, scope) {
		return nil, errDSGAStorageManagement
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Cache-Control", "no-store")
	// 无Idempotency-Key，不触发Go Transport对POST的幂等透明重试。显式重放
	// 必须由持久原意图协调器决定；本客户端只做一次Do。
	response, err := c.http.Do(req)
	if err != nil {
		return nil, errDSGAStorageManagement
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, dsgaStorageHTTPError{response.StatusCode}
	}
	types := response.Header.Values("Content-Type")
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.Header.Get("Content-Encoding") != "" || len(types) != 1 || mediaErr != nil || mediaType != "application/json" {
		return nil, errDSGAStorageManagement
	}
	limit, values := 1<<20, 65536
	validate := dsgapi.ValidateJSON
	if method == http.MethodGet && strings.Contains(path, "/timeline/") {
		// 官方聚合TimelineInfo包含每片完整观测，255片不能沿用单Tenant的
		// 1MiB上限；仍限制总字节/JSON值数，不随远端声明无限分配。
		limit, values = 8<<20, 262144
		validate = dsgapi.ValidateObservationJSON
	}
	if response.ContentLength > int64(limit) {
		return nil, errDSGAStorageManagement
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil || validate(data, limit, values) != nil {
		return nil, errDSGAStorageManagement
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var fact map[string]any
	if decoder.Decode(&fact) != nil || fact == nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errDSGAStorageManagement
	}
	return fact, nil
}
