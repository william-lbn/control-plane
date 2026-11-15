package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Run against the settled native Writer fixture before its regular public
// resume. Each admission rolls back to the same original suspension.
func testFunctionWriterResumeAdmission(t *testing.T, ctx context.Context, s *server, p createPayload, source dsgaResumeSource) {
	const actor = "resume-admin"
	const function = "fnc_c340300010000001"
	const child = "op_0123456789abcdef01234567"
	for _, automatic := range []bool{false, true} {
		name := "request-credential"
		if automatic {
			name = "current-public-policy"
		}
		t.Run(name, func(t *testing.T) {
			tx, err := s.db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			exec := func(stmt string, args ...any) {
				t.Helper()
				if _, err := tx.Exec(ctx, stmt, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`INSERT INTO function_definitions(id,org_id,project_id,branch_id,endpoint_id,slug,database_name,sql_schema,sql_role,state)
 VALUES($1,'local',$2,$3,$4,'cold','neondb','app','fn_c340300010000001','suspended')`, function, p.ProjectID, p.BranchID, p.EndpointID)
			requestCtx := context.WithValue(ctx, userKey, user{ID: actor, CSRFHash: digest("resume-csrf")})
			if automatic {
				requestCtx = context.WithValue(requestCtx, functionExpectedIdentityKey{}, function)
				exec(`INSERT INTO function_http_policies(function_id,actor_id,idle_suspended) VALUES($1,$2,true)`, function, actor)
			}
			req := httptest.NewRequest(http.MethodPost, "/resume", nil).WithContext(requestCtx)
			req.SetPathValue("project", p.ProjectID)
			req.SetPathValue("branch", p.BranchID)
			if !automatic {
				if _, err := s.lockFunctionWriterResume(req, tx, function, true); !errors.Is(err, errDSGACreationAuthority) {
					t.Fatalf("cold Writer accepted missing credential: %v", err)
				}
				req.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: "resume-session"})
			}
			writer, err := s.lockFunctionWriterResume(req, tx, function, true)
			if err != nil || writer.scope.EndpointID != p.EndpointID || writer.automatic != automatic {
				t.Fatalf("original Writer authority differs: %v", err)
			}
			if automatic {
				if _, err := s.queueFunctionWriterResume(req, tx, writer, function, child, 2); !errors.Is(err, errDSGACreationAuthority) {
					t.Fatalf("cold Writer accepted revoked public policy: %v", err)
				}
				exec(`UPDATE function_http_policies SET public_enabled=true,idle_enabled=true WHERE function_id=$1`, function)
			}
			parent, err := s.queueFunctionWriterResume(req, tx, writer, function, child, 2)
			if err != nil {
				t.Fatal(err)
			}
			var retained string
			err = tx.QueryRow(ctx, `SELECT payload->>'resume_from' FROM operations WHERE id=$1 AND resource_id=$2 AND action=$3`,
				parent, p.EndpointID, dsgaResumeAction).Scan(&retained)
			if err != nil || retained != source.Operation {
				t.Fatalf("wrong retained Writer origin: %s %v", retained, err)
			}
			exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload)
 VALUES($1,$2,'function',$3,'deploy_function','queued',$4,'function-test',jsonb_build_object('writer_resume_operation_id',$5::text,'branch_id',$6::text))`,
				child, p.ProjectID, function, actor, parent, p.BranchID)
			exec(`UPDATE operations SET state='running',lease_owner='original-writer-worker',lease_expires_at=now()+interval '1 minute' WHERE id=$1`, parent)
			var id, project, resource, action string
			var payload []byte
			if err := tx.QueryRow(ctx, nextWorkerOperationSQL).Scan(&id, &project, &resource, &action, &payload); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("Worker claimed Function ahead of original Writer: id=%s err=%v", id, err)
			}
			exec(`UPDATE operations SET state='failed',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, parent)
			if err := tx.QueryRow(ctx, nextWorkerOperationSQL).Scan(&id, &project, &resource, &action, &payload); err != nil || id != child {
				t.Fatalf("terminal Writer failure left Function queued: id=%s err=%v", id, err)
			}
		})
	}
}
