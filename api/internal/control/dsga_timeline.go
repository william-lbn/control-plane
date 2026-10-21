package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// DSGA单独采用原计划的大版本，不改变原PG16创建路径。先GET原目标以恢复未知
// POST结果；首次创建前核对祖先的真实存储身份/大版本，再提交PG17创建请求。
// 新目标即使POST成功也须GET确认；不把HTTP成功/409当作原版本与原祖先的证明。
// 每次调用只有一次POST，丢回复原样返回，交同一原Operation下一次显式恢复。
func (k *kubeClient) createDSGATimeline(ctx context.Context, p createPayload, plan dsgaCreationPlan) error {
	if plan.validate(p) != nil || plan.Namespace != k.namespace {
		return errDSGACreationPlan
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	clients, _, err := newDSGAKubeClients(k)
	if err != nil {
		return err
	}
	// 复用中心固定HTTPS/TLS/轮转SA身份、共享限流和5秒单请求期限，禁止redirect。
	// 原legacy serviceRequest不承担本协议；明确禁止client-go透明重试POST，
	// 响应按8MiB有界读取，用UseNumber保留服务端整数形状后再做严格事实验证。
	request := func(path, method string, body any) (map[string]any, error) {
		proxy := fmt.Sprintf("/api/v1/namespaces/%s/services/http:pageserver-managed:9898/proxy/%s", k.namespace, path)
		if method == http.MethodPost {
			proxy = fmt.Sprintf("/api/v1/namespaces/%s/services/http:storage-controller:1234/proxy/%s", k.namespace, path)
		}
		req := clients.CoreV1().RESTClient().Verb(method).AbsPath(proxy).MaxRetries(0)
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			req = req.Body(raw).SetHeader("Content-Type", "application/json")
		}
		stream, err := req.Stream(ctx)
		if err != nil {
			return nil, err
		}
		defer stream.Close()
		raw, err := io.ReadAll(io.LimitReader(stream, (8<<20)+1))
		if err != nil || len(raw) > 8<<20 {
			return nil, errDSGACreationIdentity
		}
		var object map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&object); err != nil {
			return nil, errDSGACreationIdentity
		}
		if decoder.Decode(new(any)) != io.EOF {
			return nil, errDSGACreationIdentity
		}
		return object, nil
	}
	path := "v1/tenant/" + p.TenantID + "/timeline/" + p.TimelineID
	target, err := request(path, http.MethodGet, nil)
	if err == nil {
		return validateDSGATimeline(target, p.TenantID, p.TimelineID, plan.PostgresVersion, p.ParentTimelineID, p.ParentLSN, true)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	if p.ParentTimelineID != "" {
		parent, err := request("v1/tenant/"+p.TenantID+"/timeline/"+p.ParentTimelineID, http.MethodGet, nil)
		if err != nil {
			return err
		}
		// 祖先自身可能也有祖先，不按新分支的ParentLSN去误判它的历史；只验证原
		// tenant/timeline/PG事实。实际保留点检查仍由原创建流程的pin_restore_point完成。
		if err := validateDSGATimeline(parent, p.TenantID, p.ParentTimelineID, plan.PostgresVersion, "", "", false); err != nil {
			return err
		}
	}
	body := map[string]any{"new_timeline_id": p.TimelineID, "pg_version": plan.PostgresVersion}
	if p.ParentTimelineID != "" {
		body["ancestor_timeline_id"], body["ancestor_start_lsn"] = p.ParentTimelineID, p.ParentLSN
	}
	_, err = request("v1/tenant/"+p.TenantID+"/timeline", http.MethodPost, body)
	if err != nil && !apierrors.IsConflict(err) {
		return err
	}
	target, err = request(path, http.MethodGet, nil)
	if err != nil {
		return err
	}
	return validateDSGATimeline(target, p.TenantID, p.TimelineID, plan.PostgresVersion, p.ParentTimelineID, p.ParentLSN, true)
}

func validateDSGATimeline(item map[string]any, tenant, timeline string, version uint32, parent, lsn string, ancestry bool) error {
	// serviceRequest当前统一JSON对象解码为map；这里严格重解码成整数，不能用
	// number()把17.5截断成17。丢字段/null/字符串版本或越界必须拒绝，零不能默认17。
	raw, err := json.Marshal(item)
	if err != nil {
		return errDSGACreationIdentity
	}
	var fact struct {
		Tenant   string `json:"tenant_id"`
		Timeline string `json:"timeline_id"`
		Version  uint32 `json:"pg_version"`
	}
	if json.Unmarshal(raw, &fact) != nil || fact.Tenant != tenant || fact.Timeline != timeline || fact.Version != version {
		return errDSGACreationIdentity
	}
	if ancestry {
		if err := validateTimelineAncestor(item, parent, lsn); err != nil {
			return errors.Join(errDSGACreationIdentity, err)
		}
	}
	return nil
}
