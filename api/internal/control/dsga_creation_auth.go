package control

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

type dsgaCreationAuthKey struct{}

// 只在当前HTTP上下文保留已验证凭据的引用，不保存Bearer/Session明文，不写
// reservation/Operation。新会话可重试同actor原意图，但本次请求必须一直有权。
type dsgaCreationAuth struct {
	Actor, KeyID, KeyOrg, KeyProject, KeyRole, SessionHash, CSRFHash string
}

func dsgaCreationRequestContext(r *http.Request) (context.Context, error) {
	u := userFrom(r)
	a := dsgaCreationAuth{Actor: u.ID}
	if u.ID == "" {
		return nil, errDSGACreationAuthority
	}
	if u.APIKey {
		if u.KeyID == "" {
			return nil, errDSGACreationAuthority
		}
		a.KeyID, a.KeyOrg, a.KeyProject, a.KeyRole = u.KeyID, u.KeyOrg, u.KeyProject, u.KeyRole
	} else {
		cookie, err := r.Cookie("neon_v2_session")
		if err != nil || cookie.Value == "" || u.CSRFHash == "" {
			return nil, errDSGACreationAuthority
		}
		a.SessionHash, a.CSRFHash = digest(cookie.Value), u.CSRFHash
	}
	return context.WithValue(r.Context(), dsgaCreationAuthKey{}, a), nil
}

func dsgaAuthorityReadError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errDSGACreationAuthority
	}
	return err
}

// 固定在org/user/member之后、quota/reservation之前取凭据行SHARE；最终提交
// 前再核验。WHERE中不预先计算到期：排队取得行锁后单独读取数据库当前时钟，
// 避免事务now()或锁等待前的clock_timestamp判断放过刚刚过期的凭据。
// 行锁只覆盖SQL事务，不跨凭据Secret网络调用；撤销可在两个阶段间生效。
func lockDSGARequestCredential(ctx context.Context, tx pgx.Tx, org, actor string) error {
	a, ok := ctx.Value(dsgaCreationAuthKey{}).(dsgaCreationAuth)
	if !ok || a.Actor != actor {
		return errDSGACreationAuthority
	}
	var owner string
	var expires time.Time
	var revoked *time.Time
	if a.KeyID != "" {
		var keyOrg, keyProject, role string
		err := tx.QueryRow(ctx, `SELECT user_id,COALESCE(org_id,''),COALESCE(project_id,''),max_role,expires_at,revoked_at FROM api_keys WHERE id=$1 FOR SHARE`, a.KeyID).
			Scan(&owner, &keyOrg, &keyProject, &role, &expires, &revoked)
		if err != nil {
			return dsgaAuthorityReadError(err)
		}
		if keyOrg != a.KeyOrg || keyProject != a.KeyProject || role != a.KeyRole || roleLevel(role) < 2 || (keyOrg != "" && keyOrg != org) || keyProject != "" {
			return errDSGACreationAuthority
		}
	} else {
		var csrf string
		err := tx.QueryRow(ctx, `SELECT user_id,csrf_hash,expires_at,revoked_at FROM sessions WHERE token_hash=$1 FOR SHARE`, a.SessionHash).Scan(&owner, &csrf, &expires, &revoked)
		if err != nil {
			return dsgaAuthorityReadError(err)
		}
		if csrf != a.CSRFHash {
			return errDSGACreationAuthority
		}
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if owner != actor || revoked != nil || !now.Before(expires) || ctx.Err() != nil {
		return errDSGACreationAuthority
	}
	return nil
}
