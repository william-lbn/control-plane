package control

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

type savedQuerySpec struct {
	Name string `json:"name"`
	SQL  string `json:"sql"`
}

func validSavedQuery(q *savedQuerySpec) bool {
	q.Name = strings.TrimSpace(q.Name)
	return len(q.Name) > 0 && len(q.Name) <= 100 && !strings.ContainsAny(q.Name, "\r\n\x00") && len(q.SQL) > 0 && len(q.SQL) <= 65536 && utf8.ValidString(q.SQL) && !strings.ContainsRune(q.SQL, '\x00')
}
func savedQueryAAD(project, owner, id string, generation int64) []byte {
	data, _ := json.Marshal([]any{"saved-query-v1", project, owner, id, generation})
	return data
}
func (s *server) savedCipher(version string) (cipher.AEAD, error) {
	if s.backendKeys == nil || s.backendKeys.decoded[version] == nil {
		return nil, errors.New("saved query encryption version unavailable")
	}
	mac := hmac.New(sha256.New, s.backendKeys.decoded[version])
	mac.Write([]byte("neon-saved-query-encryption-v1"))
	block, e := aes.NewCipher(mac.Sum(nil))
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(block)
}
func (s *server) sealSavedSQL(version, sql string, aad []byte) ([]byte, error) {
	a, e := s.savedCipher(version)
	if e != nil {
		return nil, e
	}
	nonce := make([]byte, a.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return nil, e
	}
	return a.Seal(nonce, nonce, []byte(sql), aad), nil
}
func (s *server) openSavedSQL(version string, sealed, aad []byte) (string, error) {
	a, e := s.savedCipher(version)
	if e != nil || len(sealed) < 28 {
		return "", errors.New("saved query key or envelope unavailable")
	}
	plain, e := a.Open(nil, sealed[:a.NonceSize()], sealed[a.NonceSize():], aad)
	if e != nil {
		return "", errors.New("saved query authentication failed")
	}
	return string(plain), nil
}

const savedQueryPublicColumns = `id,project_id,owner_id,name,generation,created_at,updated_at`

func (s *server) savedQueries(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.backendKeys == nil || len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "saved_queries_disabled", "Versioned encryption and durable idempotency keys are required")
		return
	}
	project, owner, id := r.PathValue("project"), userFrom(r).ID, r.PathValue("query")
	if r.Method == http.MethodGet {
		if id == "" {
			items, e := s.many(r.Context(), `SELECT `+savedQueryPublicColumns+` FROM saved_queries WHERE project_id=$1 AND owner_id=$2 AND deleted_at IS NULL ORDER BY updated_at DESC,id LIMIT 50`, project, owner)
			if e != nil {
				fail(w, r, 503, "metadata_unavailable", "Could not list saved queries")
				return
			}
			jsonResponse(w, 200, record{"items": items, "limit": 50, "visibility": "personal"})
			return
		}
		item, e := s.one(r.Context(), `SELECT `+savedQueryPublicColumns+`,key_version,sealed_sql,content_generation FROM saved_queries WHERE project_id=$1 AND owner_id=$2 AND id=$3 AND deleted_at IS NULL`, project, owner, id)
		if e != nil {
			if isNoRows(e) {
				fail(w, r, 404, "not_found", "Saved query not found")
			} else {
				fail(w, r, 503, "metadata_unavailable", "Could not read saved query")
			}
			return
		}
		sql, e := s.openSavedSQL(stringVal(item["key_version"]), item["sealed_sql"].([]byte), savedQueryAAD(project, owner, id, item["content_generation"].(int64)))
		if e != nil {
			fail(w, r, 503, "saved_query_key_unavailable", "Could not decrypt saved query")
			return
		}
		delete(item, "key_version")
		delete(item, "sealed_sql")
		delete(item, "content_generation")
		item["sql"] = sql
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, item["generation"]))
		jsonResponse(w, 200, item)
		return
	}
	idem := r.Header.Get("Idempotency-Key")
	if len(idem) < 8 || len(idem) > 128 || strings.ContainsAny(idem, "\r\n") {
		fail(w, r, 422, "invalid_idempotency_key", "Idempotency-Key must contain 8 to 128 bytes")
		return
	}
	spec := savedQuerySpec{}
	if r.Method != http.MethodDelete {
		if readJSON(r, &spec) != nil || !validSavedQuery(&spec) {
			fail(w, r, 422, "invalid_request", "Name and UTF-8 SQL are required; maximum SQL size is 64 KiB")
			return
		}
	}
	var expected int64
	if id != "" {
		tag := r.Header.Get("If-Match")
		if tag == "" {
			fail(w, r, 428, "precondition_required", "If-Match is required")
			return
		}
		if len(tag) < 3 || tag[0] != '"' || tag[len(tag)-1] != '"' {
			fail(w, r, 422, "invalid_version", "Expected a quoted generation")
			return
		}
		var e error
		expected, e = strconv.ParseInt(tag[1:len(tag)-1], 10, 64)
		if e != nil || expected < 1 {
			fail(w, r, 422, "invalid_version", "Invalid generation")
			return
		}
	}
	keyHash := s.createRequestHash("saved-query-idempotency", idem)
	requestHash := s.createRequestHash("saved-query-request", record{"method": r.Method, "path": r.URL.Path, "expected": expected, "spec": spec})
	tx, e := s.db.Begin(r.Context())
	if e != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not begin saved query change")
		return
	}
	defer tx.Rollback(r.Context())
	// One owner/project lock bounds the count and serializes personal mutations.
	// The actor/key lock makes replay safe even across different projects/routes.
	if _, e = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0)),pg_advisory_xact_lock(hashtextextended($2,0))`, "saved-key:"+owner+":"+keyHash, "saved-owner:"+owner+":"+project); e != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not lock saved query")
		return
	}
	var priorHash string
	var prior []byte
	e = tx.QueryRow(r.Context(), `SELECT request_hash,response FROM saved_query_requests WHERE owner_id=$1 AND key_hash=$2`, owner, keyHash).Scan(&priorHash, &prior)
	if e == nil {
		if priorHash != requestHash {
			fail(w, r, 409, "idempotency_conflict", "Idempotency-Key was used with different input")
			return
		}
		var reply record
		if json.Unmarshal(prior, &reply) != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read replay")
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		jsonResponse(w, 200, reply)
		return
	} else if !errors.Is(e, pgx.ErrNoRows) {
		fail(w, r, 503, "metadata_unavailable", "Could not check replay")
		return
	}
	var projectState string
	if e = tx.QueryRow(r.Context(), `SELECT state FROM projects WHERE id=$1 AND deleted_at IS NULL FOR SHARE`, project).Scan(&projectState); e != nil || projectState != "ready" {
		fail(w, r, 409, "project_not_ready", "Project must be ready")
		return
	}
	generation := int64(1)
	action := "create_saved_query"
	if id != "" {
		if e = tx.QueryRow(r.Context(), `SELECT generation FROM saved_queries WHERE project_id=$1 AND owner_id=$2 AND id=$3 AND deleted_at IS NULL FOR UPDATE`, project, owner, id).Scan(&generation); e != nil {
			if errors.Is(e, pgx.ErrNoRows) {
				fail(w, r, 404, "not_found", "Saved query not found")
			} else {
				fail(w, r, 503, "metadata_unavailable", "Could not lock saved query")
			}
			return
		}
		if expected != generation {
			fail(w, r, 412, "version_conflict", "Saved query changed; reload before editing")
			return
		}
		generation++
		action = "update_saved_query"
	} else {
		var count int
		if tx.QueryRow(r.Context(), `SELECT count(*) FROM saved_queries WHERE project_id=$1 AND owner_id=$2 AND deleted_at IS NULL`, project, owner).Scan(&count) != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read saved query quota")
			return
		}
		if count >= 50 {
			fail(w, r, 409, "saved_query_limit", "At most 50 personal queries per project")
			return
		}
		id = newID("sq_")
	}
	version := s.backendKeys.Active
	var item record
	if r.Method == http.MethodDelete {
		action = "delete_saved_query"
		rows, err := tx.Query(r.Context(), `UPDATE saved_queries SET deleted_at=now(),updated_at=now(),generation=$4 WHERE project_id=$1 AND owner_id=$2 AND id=$3 RETURNING `+savedQueryPublicColumns, project, owner, id, generation)
		if err == nil {
			item, e = pgx.CollectOneRow(rows, pgx.RowToMap)
		} else {
			e = err
		}
	} else {
		var sealed []byte
		sealed, e = s.sealSavedSQL(version, spec.SQL, savedQueryAAD(project, owner, id, generation))
		if e != nil {
			fail(w, r, 503, "saved_query_key_unavailable", "Could not encrypt saved query")
			return
		}
		var rows pgx.Rows
		if generation == 1 {
			rows, e = tx.Query(r.Context(), `INSERT INTO saved_queries(id,project_id,owner_id,name,key_version,sealed_sql) VALUES($1,$2,$3,$4,$5,$6) RETURNING `+savedQueryPublicColumns, id, project, owner, spec.Name, version, sealed)
		} else {
			rows, e = tx.Query(r.Context(), `UPDATE saved_queries SET name=$4,key_version=$5,sealed_sql=$6,generation=$7,content_generation=$7,updated_at=now() WHERE project_id=$1 AND owner_id=$2 AND id=$3 RETURNING `+savedQueryPublicColumns, project, owner, id, spec.Name, version, sealed, generation)
		}
		if e == nil {
			item, e = pgx.CollectOneRow(rows, pgx.RowToMap)
		}
	}
	if e != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not change saved query")
		return
	}
	reply := record{"query": item, "sql_included": false}
	encoded, _ := json.Marshal(reply)
	if _, e = tx.Exec(r.Context(), `INSERT INTO saved_query_requests(owner_id,key_hash,request_hash,response) VALUES($1,$2,$3,$4)`, owner, keyHash, requestHash, encoded); e != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not record saved query replay")
		return
	}
	if _, e = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,outcome,request_id) VALUES($1,$2,'saved_query',$3,'succeeded',$4)`, owner, action, id, requestID(r)); e != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not audit saved query change")
		return
	}
	if tx.Commit(r.Context()) != nil {
		fail(w, r, 503, "metadata_unavailable", "Saved query outcome unavailable; retry with the same key")
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, generation))
	status := 200
	if generation == 1 {
		status = 201
	}
	jsonResponse(w, status, reply)
}
