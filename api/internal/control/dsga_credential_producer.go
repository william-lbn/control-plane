package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
	"github.com/xdg-go/stringprep"
	"golang.org/x/text/secure/precis"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	dsgaCredentialOperation   = "neon-control/dsga-creation-operation"
	dsgaCredentialIncarnation = "neon-control/dsga-creation-incarnation"
	dsgaCredentialCompute     = "neon-control/dsga-creation-compute-instance"
)

// 此输入只供可信创建生产者使用，不是公开HTTP请求结构。复制/只读实例必须
// 由原catalog选择来源endpoint和已冻结的UID/摘要，再实际GET该来源；不能仅凭
// 同项目标签、任意Secret指针或租户密码重造物理副本的pg_authid内容。
type dsgaCredentialInput struct {
	Password       string
	SourceEndpoint string
	Source         dsgaCredentialReference
}

func dsgaCredentialProvenance(secret *core.Secret, operation string, plan dsgaCreationPlan) bool {
	return secret != nil && secret.Annotations[dsgaCredentialOperation] == operation &&
		secret.Annotations[dsgaCredentialIncarnation] == plan.Incarnation &&
		secret.Annotations[dsgaCredentialCompute] == plan.ComputeInstanceID
}

// prepareDSGACredentials必须在原Operation接受事务之前执行，返回值仅含公开
// 引用；调用方把完整plan交addDSGACreateOperation，不能在worker首次读取时收养。
// 这里不持数据库锁，不授予预算/VM许可，不写旧mutable adminVerifier路径。
// 重试先GET并验证原Operation/实例、口令或继承来源；存在即不生成随机数、不
// 更新元数据。已冻结UID的对象丢失时拒绝重建。首次POST未知结果由下次原意图
// GET对账，不能自动重试POST或更换原Operation；孤立对象也不自动删除/收养。
// 公开请求的幂等记录、来源catalog关系与失败清理仍由接受层负责。
func (k *kubeClient) prepareDSGACredentials(ctx context.Context, p createPayload, plan dsgaCreationPlan, operation string, input dsgaCredentialInput) (dsgaCreationPlan, error) {
	var empty dsgaCreationPlan
	if k == nil || plan.validate(p) != nil || plan.Namespace != k.namespace || dsgapi.ValidateID(operation, "creation Operation") != nil {
		return empty, errDSGACredentialSource
	}
	inherit := input.Source != (dsgaCredentialReference{})
	if inherit {
		if input.Password != "" || input.Source.validate() != nil || input.SourceEndpoint == p.EndpointID || dsgapi.ValidateID(input.SourceEndpoint, "source endpoint") != nil {
			return empty, errDSGACredentialSource
		}
	} else if input.SourceEndpoint != "" || p.EndpointType != "read_write" || p.ParentTimelineID != "" || p.RestoreSource != "" {
		return empty, errDSGACredentialSource
	} else if _, ok := dsgaPassword(input.Password); !ok {
		return empty, errDSGACredentialSource
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	clients, _, err := newDSGAKubeClients(k)
	if err != nil {
		return empty, errDSGACredentialSource
	}
	var inherited dsgaRenderCredentials
	if inherit {
		sourceName := kubeName(input.SourceEndpoint) + "-credentials"
		source, err := clients.CoreV1().Secrets(plan.Namespace).Get(ctx, sourceName, metav1.GetOptions{})
		if err != nil {
			return empty, errDSGACredentialSource
		}
		snapshot, err := snapshotDSGACredentials(source, plan.Namespace, sourceName, p.ProjectID, input.SourceEndpoint)
		if err != nil || snapshot.reference() != input.Source {
			return empty, errDSGACredentialSource
		}
		inherited = snapshot.credentials
	}
	name := kubeName(p.EndpointID) + "-credentials"
	confirm := func(secret *core.Secret) (dsgaCreationPlan, error) {
		snapshot, err := snapshotDSGACredentials(secret, plan.Namespace, name, p.ProjectID, p.EndpointID)
		if err != nil || !dsgaCredentialProvenance(secret, operation, plan) ||
			(plan.Credentials != (dsgaCredentialReference{}) && snapshot.reference() != plan.Credentials) {
			return empty, errDSGACredentialSource
		}
		if inherit {
			if snapshot.credentials.ApplicationVerifier != inherited.ApplicationVerifier || snapshot.credentials.ProbeVerifier != inherited.ProbeVerifier {
				return empty, errDSGACredentialSource
			}
		} else if !dsgaPasswordMatches(input.Password, snapshot.credentials.ApplicationVerifier) {
			return empty, errDSGACredentialSource
		}
		frozen := plan
		frozen.Credentials = snapshot.reference()
		return frozen, nil
	}
	current, err := clients.CoreV1().Secrets(plan.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return confirm(current)
	}
	if !apierrors.IsNotFound(err) || plan.Credentials != (dsgaCredentialReference{}) {
		return empty, errDSGACredentialSource
	}
	data, err := makeDSGACredentialData(input.Password, inherited, inherit, rand.Reader)
	if err != nil {
		return empty, errDSGACredentialSource
	}
	immutable := true
	proposed := &core.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: plan.Namespace, Name: name,
		Labels: credentialLabels(p.ProjectID, p.EndpointID), Annotations: map[string]string{
			dsgaCredentialOperation: operation, dsgaCredentialIncarnation: plan.Incarnation, dsgaCredentialCompute: plan.ComputeInstanceID,
		}}, Type: core.SecretTypeOpaque, Immutable: &immutable, Data: data}
	var created core.Secret
	// typed Create默认会透明重试429；这个一次性写入明确关闭重试。错误不带
	// 原API响应，防止异常服务把Secret body反射进日志。冲突只能GET原赢家。
	err = clients.CoreV1().RESTClient().Post().Namespace(plan.Namespace).Resource("secrets").Body(proposed).MaxRetries(0).Do(ctx).Into(&created)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return empty, errDSGACredentialSource
	}
	conflict := err != nil
	current, err = clients.CoreV1().Secrets(plan.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return empty, errDSGACredentialSource
	}
	if !conflict {
		// 成功响应也必须读回实际UID和完整Data；不能只相信POST响应或标签。
		// 409的同原意图赢家可能使用另一份随机盐，改由confirm验证口令/来源。
		if created.UID == "" || current.UID != created.UID || len(current.Data) != len(data) {
			return empty, errDSGACredentialSource
		}
		for key, value := range data {
			if !bytes.Equal(current.Data[key], value) {
				return empty, errDSGACredentialSource
			}
		}
	}
	return confirm(current)
}

// PostgreSQL/libpq采用SASLprep，而当前固定pgx采用PRECIS；二者对部分Unicode
// 并不相同。只接受两条真实客户端产生相同字节的口令，否则在接受前拒绝，避免
// 生成部分客户端永远无法认证的immutable verifier。PG标准的准备失败回退原文
// 也保留。长度按原输入字节限制，拒绝NUL/非法UTF-8，明文不进入Secret或plan。
func dsgaPassword(password string) (string, bool) {
	if len(password) < 12 || len(password) > 256 || !utf8.ValidString(password) || strings.ContainsRune(password, 0) {
		return "", false
	}
	// PG先映射非ASCII空格，再映射B.1；U+200B同时在两表，顺序不能反过来。
	spaces := stringprep.Mapping{}
	for _, ch := range []rune{0xa0, 0x1680, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a, 0x200b, 0x202f, 0x205f, 0x3000} {
		spaces[ch] = []rune{' '}
	}
	profile := stringprep.SASLprep
	profile.Mappings = []stringprep.Mapping{spaces, stringprep.TableB1}
	prepared, err := profile.Prepare(password)
	if err != nil {
		prepared = password
	}
	client, err := precis.OpaqueString.String(password)
	if err != nil {
		client = password
	}
	return prepared, prepared == client
}

func dsgaVerifier(password string, salt []byte) string {
	salted := pbkdf2(password, salt, 4096, sha256.Size)
	client := hmac.New(sha256.New, salted)
	_, _ = client.Write([]byte("Client Key"))
	stored := sha256.Sum256(client.Sum(nil))
	server := hmac.New(sha256.New, salted)
	_, _ = server.Write([]byte("Server Key"))
	return fmt.Sprintf("SCRAM-SHA-256$4096:%s$%s:%s", base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(stored[:]), base64.StdEncoding.EncodeToString(server.Sum(nil)))
}

func dsgaPasswordMatches(password, verifier string) bool {
	prepared, ok := dsgaPassword(password)
	if !ok || !functionsql.ValidVerifier(verifier) {
		return false
	}
	salt, err := base64.StdEncoding.Strict().DecodeString(strings.Split(strings.Split(verifier, "$")[1], ":")[1])
	// 比对整个规范verifier，同时验证StoredKey和ServerKey；仅验证StoredKey
	// 会把服务端证明已损坏的凭据误认作一次成功重试。
	return err == nil && hmac.Equal([]byte(dsgaVerifier(prepared, salt)), []byte(verifier))
}

func makeDSGACredentialData(password string, source dsgaRenderCredentials, inherit bool, random io.Reader) (map[string][]byte, error) {
	var entropy [96]byte
	count := len(entropy)
	if inherit {
		count = ed25519.SeedSize
		if !functionsql.ValidVerifier(source.ApplicationVerifier) || !functionsql.ValidVerifier(source.ProbeVerifier) {
			return nil, errDSGACredentialSource
		}
	}
	prepared := ""
	if !inherit {
		var ok bool
		prepared, ok = dsgaPassword(password)
		if !ok {
			return nil, errDSGACredentialSource
		}
	}
	// 先完整取得所需随机字节；失败不返回半份凭据，也不会触发Kube写入。
	if _, err := io.ReadFull(random, entropy[:count]); err != nil {
		return nil, errDSGACredentialSource
	}
	app, probe := source.ApplicationVerifier, source.ProbeVerifier
	if !inherit {
		app = dsgaVerifier(prepared, entropy[64:80])
		probe = dsgaVerifier(base64.RawURLEncoding.EncodeToString(entropy[32:64]), entropy[80:96])
	}
	return map[string][]byte{"applicationVerifier": []byte(app), "probeVerifier": []byte(probe), "controlSigningSeed": append([]byte(nil), entropy[:ed25519.SeedSize]...)}, nil
}
