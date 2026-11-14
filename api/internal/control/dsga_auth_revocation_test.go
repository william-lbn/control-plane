package control

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func testSuspendedDSGAServiceAdmission(t *testing.T, ctx context.Context, b *dsgaCreationBinding) {
	t.Helper()
	for _, service := range []string{"managed_auth", "data_api"} {
		t.Run(service, func(t *testing.T) { testSuspendedDSGAService(t, ctx, b, service) })
	}
}

func testSuspendedDSGAService(t *testing.T, ctx context.Context, b *dsgaCreationBinding, service string) {
	t.Helper()
	tx, err := b.server.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	p := b.intent.createPayload
	if _, err = tx.Exec(ctx, `INSERT INTO `+pgQuote(service+"_instances")+`(branch_id,project_id,endpoint_id,generation,state,spec,secret_ref)
 VALUES($1,$2,$3,8,'disabling','{"database":"neondb"}','auth-previous')`, p.BranchID, p.ProjectID, p.EndpointID); err != nil {
		t.Fatal(err)
	}
	const query = `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload)
 VALUES($1,$2,$7,$3,$4,'queued','service-fixture','service-fixture',
 jsonb_build_object('project_id',$2::text,'branch_id',$3::text,'endpoint_id',$5::text,'generation',$6::int,
 'secret_ref','auth-previous','spec','{"database":"neondb"}'::jsonb))`
	for _, tc := range []struct {
		action     string
		generation int
	}{{"enable_" + service, 8}, {"disable_" + service, 7}} {
		attempt, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = attempt.Exec(ctx, query, "auth-negative", p.ProjectID, p.BranchID, tc.action, p.EndpointID, tc.generation, service)
		_ = attempt.Rollback(ctx)
		var denied *pgconn.PgError
		if !errors.As(err, &denied) || denied.Code != "23514" {
			t.Fatalf("unexpected admission for %s at generation %d: %v", tc.action, tc.generation, err)
		}
	}
	if _, err = tx.Exec(ctx, query, "suspended-"+service, p.ProjectID, p.BranchID, "disable_"+service, p.EndpointID, 8, service); err != nil {
		t.Fatal(err)
	}
}

func TestDSGAAuthRevocationReadsOnlyExactRetainedWriter(t *testing.T) {
	ctx, binding, _ := dsgaSuspensionAdmissionFixture(t)
	db := binding.server.db
	scope := binding.intent.createPayload
	read := func(wanted createPayload, found bool, wantErr error) {
		t.Helper()
		p, disabled, err := readDisabledDSGAAuth(ctx, db, wanted)
		if disabled != found || !errors.Is(err, wantErr) {
			t.Fatalf("disabled=%t, error=%v; want %t, %v", disabled, err, found, wantErr)
		}
		if found && (p.Generation != 4 || p.Spec.Database != "neondb" || p.SecretRef != "original-auth") {
			t.Fatalf("stored disable intent not returned: %+v", p)
		}
	}
	read(scope, false, nil)
	if _, err := db.Exec(ctx, `INSERT INTO managed_auth_instances(branch_id,project_id,endpoint_id,generation,state,spec,secret_ref)
 VALUES($1,$2,$3,4,'disabled','{"database":"neondb"}','original-auth')`, scope.BranchID, scope.ProjectID, scope.EndpointID); err != nil {
		t.Fatal(err)
	}
	read(scope, true, nil)
	other := scope
	other.EndpointID = "ep_ffffffffffffffff"
	read(other, false, nil)
	for _, tc := range []struct {
		state, spec string
		err         error
	}{
		{"active", `{"database":"neondb"}`, nil},
		{"disabling", `{"database":"neondb"}`, errDSGACreationIdentity},
		{"disabled", `{"database":""}`, errDSGACreationIdentity},
	} {
		if _, err := db.Exec(ctx, `UPDATE managed_auth_instances SET state=$2,spec=$3::jsonb WHERE branch_id=$1`, scope.BranchID, tc.state, tc.spec); err != nil {
			t.Fatal(err)
		}
		read(scope, false, tc.err)
	}
}
