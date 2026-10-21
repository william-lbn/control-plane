package control

import (
	"context"
	"errors"
	"github.com/william-lbn/control-plane/api/internal/functions"
	"time"
)

func (s *server) runFunctionOutputCollector(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Fairness comes from the last attempt, including unavailable instances.
			rows, e := s.db.Query(ctx, `SELECT f.id,f.project_id,f.branch_id,f.slug FROM function_output_policies p JOIN function_definitions f ON f.id=p.function_id WHERE p.enabled AND f.state='active' ORDER BY p.last_checked_at NULLS FIRST,f.id LIMIT 8`)
			if e != nil {
				continue
			}
			type target struct{ id, project, branch, slug string }
			targets := []target{}
			for rows.Next() {
				var v target
				if e = rows.Scan(&v.id, &v.project, &v.branch, &v.slug); e != nil {
					break
				}
				targets = append(targets, v)
			}
			if e == nil {
				e = rows.Err()
			}
			rows.Close()
			if e != nil {
				continue
			}
			for _, v := range targets {
				call, done := context.WithTimeout(ctx, 15*time.Second)
				runtime, err := s.loadPublishedFunction(call, v.project, v.branch, v.slug)
				if err == nil && runtime.target.FunctionID == v.id {
					client, _, e := s.observePublishedFunction(call, runtime)
					err = e
					if e == nil {
						err = s.collectFunctionOutputClient(call, runtime.target, runtime.instance.Scope.InstanceID, runtime.bootID, client)
						client.Close()
					}
				}
				done()
				code := ""
				if err != nil {
					code = "output_collection_unavailable"
				}
				s.db.Exec(ctx, `UPDATE function_output_policies SET last_checked_at=clock_timestamp(),last_error_code=NULLIF($2,'') WHERE function_id=$1`, v.id, code)
			}
			// Only acknowledged customer-log ciphertext expires here. Object bytes,
			// Function artifacts and request/operation histories are never selected.
			s.db.Exec(ctx, `DELETE FROM function_output_records WHERE (instance_id,boot_id,sequence) IN (SELECT instance_id,boot_id,sequence FROM function_output_records WHERE expires_at<=now() ORDER BY expires_at LIMIT 1000)`)
		}
	}
}
func (s *server) collectFunctionOutputClient(ctx context.Context, p functionInstanceTarget, instance, boot string, client *functions.InstanceClient) error {
	if !s.functionOutputEnabled() {
		return nil
	}
	var enabled bool
	var actor string
	err := s.db.QueryRow(ctx, `SELECT enabled,actor_id FROM function_output_policies WHERE function_id=$1 AND project_id=$2 AND branch_id=$3`, p.FunctionID, p.ProjectID, p.BranchID).Scan(&enabled, &actor)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	access, e := s.projectAccess(ctx, p.ProjectID, actor)
	var allowed bool
	if e != nil || access.Level < 2 || s.db.QueryRow(ctx, `SELECT NOT disabled FROM users WHERE id=$1`, actor).Scan(&allowed) != nil || !allowed {
		return errors.New("output capture authorization revoked")
	}
	caps, e := client.Capabilities(ctx)
	if e != nil || !caps.OutputLogs || caps.BootID != boot {
		return errors.New("output runtime upgrade required")
	}
	// Four bounded pages per pass. The journal's dropped count exposes overflow.
	for batch := 0; batch < 4; batch++ {
		var after int64
		err = s.db.QueryRow(ctx, `SELECT sequence FROM function_output_cursors WHERE instance_id=$1 AND boot_id=$2`, instance, boot).Scan(&after)
		if err != nil && !isNoRows(err) {
			return err
		}
		page, e := client.Output(ctx, boot, after)
		if e != nil {
			return e
		}
		if e = s.storeFunctionOutputPage(ctx, p, instance, boot, after, page); e != nil {
			return e
		}
		if len(page.Records) == 0 {
			return nil
		}
	}
	return nil
}
func (s *server) storeFunctionOutputPage(ctx context.Context, p functionInstanceTarget, instance, boot string, after int64, page functions.OutputPage) error {
	if page.Scope.InstanceID != instance || page.Scope.DeploymentID != p.DeploymentID || page.Scope.ProjectID != p.ProjectID || page.Scope.BranchID != p.BranchID || page.Scope.Generation != p.Generation || page.Scope.Slug != p.Slug || page.BootID != boot || page.Next < after || page.Dropped < 0 || len(page.Records) > functions.MaxOutputPage {
		return errors.New("output scope rejected")
	}
	tx, e := s.db.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var enabled bool
	var since time.Time
	e = tx.QueryRow(ctx, `SELECT enabled,capture_since FROM function_output_policies WHERE function_id=$1 AND project_id=$2 AND branch_id=$3 FOR UPDATE`, p.FunctionID, p.ProjectID, p.BranchID).Scan(&enabled, &since)
	if isNoRows(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if !enabled {
		return nil
	}
	var owned bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM function_instances WHERE id=$1 AND function_id=$2 AND deployment_id=$3 AND project_id=$4 AND branch_id=$5 AND generation=$6 AND boot_id=$7)`, instance, p.FunctionID, p.DeploymentID, p.ProjectID, p.BranchID, p.Generation, boot).Scan(&owned); e != nil {
		return e
	}
	if !owned {
		return errors.New("original output instance identity changed")
	}
	if _, e = tx.Exec(ctx, `INSERT INTO function_output_cursors(instance_id,boot_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, instance, boot); e != nil {
		return e
	}
	var current int64
	if e = tx.QueryRow(ctx, `SELECT sequence FROM function_output_cursors WHERE instance_id=$1 AND boot_id=$2 FOR UPDATE`, instance, boot).Scan(&current); e != nil {
		return e
	}
	if current < after {
		return errors.New("output checkpoint moved backwards")
	}
	if current > after {
		// A periodic collector can commit a prefix while shutdown returns its
		// final page. Merge the authenticated suffix under this same cursor lock;
		// the supervisor may already be gone and cannot serve a second read.
		if page.Next <= current {
			return nil
		}
		remaining := []functions.OutputRecord{}
		for _, record := range page.Records {
			if record.Sequence > current {
				remaining = append(remaining, record)
			}
		}
		if len(remaining) == 0 {
			return errors.New("output suffix missing")
		}
		page.Records = remaining
		page.Dropped = remaining[0].Sequence - current - 1
		after = current
	}
	last := after + page.Dropped
	for _, v := range page.Records {
		if v.Sequence != last+1 || v.Timestamp.IsZero() || v.Timestamp.After(time.Now().Add(5*time.Minute)) || len(v.Message) > functions.MaxOutputLine {
			return errors.New("output sequence or line rejected")
		}
		last = v.Sequence
		if v.Timestamp.Before(since) {
			continue
		}
		ciphertext, e := s.sealFunctionOutput(p.FunctionID, instance, boot, v.Sequence, v.Message)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `INSERT INTO function_output_records(instance_id,boot_id,sequence,function_id,deployment_id,project_id,branch_id,generation,emitted_at,stream,severity,scope_name,truncated,ciphertext) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT DO NOTHING`, instance, boot, v.Sequence, p.FunctionID, p.DeploymentID, p.ProjectID, p.BranchID, p.Generation, v.Timestamp, v.Stream, v.Severity, v.ScopeName, v.Truncated, ciphertext)
		if e != nil {
			return e
		}
	}
	if last != page.Next {
		return errors.New("output page cursor mismatch")
	}
	if _, e = tx.Exec(ctx, `UPDATE function_output_cursors SET sequence=$3,dropped_lines=dropped_lines+$4,updated_at=clock_timestamp() WHERE instance_id=$1 AND boot_id=$2`, instance, boot, page.Next, page.Dropped); e != nil {
		return e
	}
	// The acknowledged capacity rule is the newest 10,000 lines per Function.
	if _, e = tx.Exec(ctx, `DELETE FROM function_output_records WHERE function_id=$1 AND (instance_id,boot_id,sequence) IN (SELECT instance_id,boot_id,sequence FROM function_output_records WHERE function_id=$1 ORDER BY captured_at DESC,instance_id DESC,sequence DESC OFFSET 10000)`, p.FunctionID); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
