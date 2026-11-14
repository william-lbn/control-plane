package control

import (
	"errors"
	"testing"
)

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
