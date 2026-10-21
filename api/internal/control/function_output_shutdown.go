package control

import (
	"context"
	"github.com/william-lbn/control-plane/api/internal/functions"
	"time"
)

// Normal retirement remains available on old Guests or when output is off.
// Once shutdown succeeds, its final tail is persisted from that response; no
// second connection to a supervisor that is exiting is needed.
func (s *server) shutdownFunctionWithOutput(ctx context.Context, p functionInstanceTarget, instance, boot string, client *functions.InstanceClient) bool {
	if !s.functionOutputEnabled() {
		return false
	}
	var enabled bool
	if s.db.QueryRow(ctx, `SELECT enabled FROM function_output_policies WHERE function_id=$1 AND project_id=$2 AND branch_id=$3`, p.FunctionID, p.ProjectID, p.BranchID).Scan(&enabled) != nil || !enabled {
		return false
	}
	pre, done := context.WithTimeout(ctx, 10*time.Second)
	err := s.collectFunctionOutputClient(pre, p, instance, boot, client)
	done()
	if err != nil {
		return false
	}
	var after int64
	err = s.db.QueryRow(ctx, `SELECT sequence FROM function_output_cursors WHERE instance_id=$1 AND boot_id=$2`, instance, boot).Scan(&after)
	if err != nil && !isNoRows(err) {
		return false
	}
	page, err := client.ShutdownOutput(ctx, boot, after)
	if err != nil {
		return false
	}
	if err = s.storeFunctionOutputPage(ctx, p, instance, boot, after, page); err != nil {
		s.db.Exec(ctx, `UPDATE function_output_policies SET last_error_code='output_final_capture_unconfirmed',last_checked_at=clock_timestamp() WHERE function_id=$1`, p.FunctionID)
	}
	return true
}
