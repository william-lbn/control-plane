package control

import "net/http"

// Delete only a personal, confirmed terminal journal entry. This never executes
// SQL or changes its database effects. Missing and foreign-owner IDs both return
// 204, allowing an interrupted DELETE to be retried without an ownership oracle.
func (s *server) deleteSQLHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u := userFrom(r)
	if u.APIKey {
		fail(w, r, 403, "console_session_required", "Console login required")
		return
	}
	ack := r.URL.Query()["acknowledge_history_deletion"]
	if len(ack) != 1 || ack[0] != "true" {
		fail(w, r, 422, "deletion_acknowledgement_required", "Explicit personal history deletion acknowledgement required")
		return
	}
	project, id := r.PathValue("project"), r.PathValue("history")
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not delete personal history")
		return
	}
	defer tx.Rollback(r.Context())
	var outcome string
	var finished bool
	err = tx.QueryRow(r.Context(), `SELECT outcome,finished_at IS NOT NULL FROM sql_query_history WHERE project_id=$1 AND owner_id=$2 AND id=$3 FOR UPDATE`, project, u.ID, id).Scan(&outcome, &finished)
	if isNoRows(err) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not lock personal history")
		return
	}
	if !finished || (outcome != "completed" && outcome != "failed" && outcome != "not_submitted") {
		fail(w, r, 409, "history_outcome_unconfirmed", "In-flight or unconfirmed history must be retained; verify the original database outcome")
		return
	}
	_, err = tx.Exec(r.Context(), `DELETE FROM sql_query_history WHERE project_id=$1 AND owner_id=$2 AND id=$3`, project, u.ID, id)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,'delete_sql_history','sql_history',$2,'succeeded',$3)`, u.ID, id, requestID(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail(w, r, 503, "history_delete_unconfirmed", "Deletion outcome unconfirmed; reload history or retry DELETE for this exact ID. Do not replay SQL")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
