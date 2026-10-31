package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/neondatabase/autoscaling/pkg/dsga/owner"
)

const dsgaArrayAuthorityScope = "compute_ctl:dsga:array-authority"

// 数组凭据是PG中立的精确请求绑定；不能用通用compute_ctl:dsga令牌替代。
// 正文hash必须由当前可信coordinator在Start CAS后canonical编码得到，不接受Guest代理权限。
// issuer这里只接受已经完成Pod/Lease/journal认证的主体；不解析或自造数组预算/许可。
type dsgaArrayRequestBinding struct {
	ProtocolVersion uint32 `json:"protocol_version"`
	Method          string `json:"method"`
	Path            string `json:"path"`
	BodySHA256      string `json:"body_sha256"`
}
type dsgaArrayCredentialEnvelope struct {
	ProtocolVersion uint32                  `json:"protocol_version"`
	Owner           owner.Ownership         `json:"owner"`
	Request         dsgaArrayRequestBinding `json:"request"`
}

func (b dsgaArrayRequestBinding) validate() error {
	if b.ProtocolVersion != 1 || b.Method != http.MethodPost {
		return fmt.Errorf("array request version/method rejected")
	}
	switch b.Path {
	case "/dsg/v1/resource/array-authorize", "/dsg/v1/resource/array-observe", "/dsg/v1/resource/array-replay-original":
	default:
		return fmt.Errorf("array request path rejected")
	}
	if len(b.BodySHA256) != 64 || b.BodySHA256 == strings.Repeat("0", 64) {
		return fmt.Errorf("array request digest rejected")
	}
	for _, c := range []byte(b.BodySHA256) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return fmt.Errorf("array request digest is not canonical lower hex")
		}
	}
	return nil
}

// 仅新v1 envelope拒重复key；旧通用入口的JSON/响应合同逐字保留，不隐式升级。
func decodeDSGAArrayCredentialEnvelope(raw []byte) (dsgaArrayCredentialEnvelope, error) {
	var e dsgaArrayCredentialEnvelope
	if len(raw) == 0 || len(raw) > 8*1024 {
		return e, fmt.Errorf("array credential body outside bound")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 16 {
			return fmt.Errorf("array credential nesting rejected")
		}
		v, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := v.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return fmt.Errorf("array credential duplicate key")
				}
				seen[s] = true
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		case '[':
			for d.More() {
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		default:
			return fmt.Errorf("array credential delimiter rejected")
		}
	}
	if err := walk(0); err != nil {
		return e, err
	}
	if _, err := d.Token(); err != io.EOF {
		return e, fmt.Errorf("array credential trailing document")
	}

	// stdlib DisallowUnknownFields仍会大小写匹配struct字段，不能作为wire精确键检查。
	// 新envelope逐层只准协议固定必填键，拒null和同义双拼写；旧Ownership入口不改。
	top, err := dsgaArrayExactObject(raw, "protocol_version", "owner", "request")
	if err != nil {
		return e, err
	}
	if _, err = dsgaArrayExactObject(top["owner"], "namespace", "vm_name", "vm_uid", "incarnation", "lease_uid", "holder_id", "issuer_epoch"); err != nil {
		return e, err
	}
	if _, err = dsgaArrayExactObject(top["request"], "protocol_version", "method", "path", "body_sha256"); err != nil {
		return e, err
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&e); err != nil {
		return e, err
	}
	if e.ProtocolVersion != 1 {
		return e, fmt.Errorf("array credential protocol rejected")
	}
	return e, e.Request.validate()
}
func (broker *dsgaCredentialBroker) serveArrayCredentials(w http.ResponseWriter, r *http.Request) {
	broker.serveCredentials(w, r, true)
}

// 该helper在全树重复key扫描之后调用；大小写/别名键均不能借struct匹配规则覆盖。
func dsgaArrayExactObject(raw []byte, keys ...string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) != len(keys) {
		return nil, fmt.Errorf("array credential exact object fields rejected")
	}
	for _, key := range keys {
		value, ok := object[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("array credential missing/null/noncanonical field")
		}
	}
	return object, nil
}
