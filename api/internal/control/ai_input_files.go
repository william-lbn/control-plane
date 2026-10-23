package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

var aiInputID = regexp.MustCompile(`^nif_[a-f0-9]{24}$`)

const aiInputPublicColumns = `id,'file' AS object,filename,content_type,purpose,bytes,sha256,extract(epoch FROM created_at)::bigint AS created_at,extract(epoch FROM expires_at)::bigint AS expires_at,'processed' AS status`
const aiInputPrivateColumns = `id,project_id,org_id,branch_id,actor_id,filename,content_type,purpose,sha256,key_version,bytes,ciphertext`

func inputFilenameValid(value string) bool {
	if !utf8.ValidString(value) || len(value) == 0 || utf8.RuneCountInString(value) > 128 || strings.TrimSpace(value) != value || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

func (s *server) aiInputMutation(w http.ResponseWriter, r *http.Request, actor, project, branch string, input any, fn func(pgx.Tx) (record, int, error)) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 {
		aiInputFail(w, r, 422, "invalid_idempotency_key", "Idempotency-Key must contain 8-128 characters")
		return
	}
	if s.backendKeys == nil || s.backendKeys.decoded[s.backendKeys.Active] == nil {
		aiInputFailure(w, r, errors.New("AI input key unavailable"))
		return
	}
	raw, err := json.Marshal([]any{r.Method, r.URL.Path, project, branch, input})
	if err != nil {
		aiInputFailure(w, r, err)
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		aiInputFailure(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	if err = aiBranchLock(r.Context(), tx, branch); err != nil {
		aiInputFailure(w, r, err)
		return
	}
	var org string
	if err = tx.QueryRow(r.Context(), `SELECT org_id FROM ai_input_file_scopes WHERE branch_id=$1 AND project_id=$2`, branch, project).Scan(&org); err != nil {
		if isNoRows(err) {
			aiInputFail(w, r, 404, "not_found", "AI input branch is not ready")
		} else {
			aiInputFailure(w, r, err)
		}
		return
	}
	var version, stored string
	var response []byte
	var status int
	err = tx.QueryRow(r.Context(), `SELECT key_version,request_hash,response,status FROM ai_input_mutations WHERE actor_id=$1 AND key_hash=$2`, actor, keyHash(key)).Scan(&version, &stored, &response, &status)
	if err == nil {
		if s.backendKeys.decoded[version] == nil {
			aiInputFailure(w, r, errors.New("AI input replay key unavailable"))
			return
		}
		if stored != s.backendHash("neon-ai-input-mutation-v1", string(raw), version) {
			aiInputFail(w, r, 409, "idempotency_conflict", "Key used for different AI input data")
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(response)
		return
	}
	if !isNoRows(err) {
		aiInputFailure(w, r, err)
		return
	}
	value, status, err := fn(tx)
	if err != nil {
		aiInputFailure(w, r, err)
		return
	}
	response, err = json.Marshal(value)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO ai_input_mutations(actor_id,key_hash,key_version,request_hash,response,status) VALUES($1,$2,$3,$4,$5,$6)`, actor, keyHash(key), s.backendKeys.Active, s.backendHash("neon-ai-input-mutation-v1", string(raw), s.backendKeys.Active), response, status)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		aiInputFailure(w, r, err)
		return
	}
	jsonResponse(w, status, value)
}

func readAIInputUpload(r *http.Request) (name, contentType, purpose string, data []byte, err error) {
	purpose = "user_data"
	parent, params, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil {
		err = e
		return
	}
	read := func(reader io.Reader) ([]byte, error) {
		value, e := io.ReadAll(io.LimitReader(reader, aigateway.MaxInputAssetBytes+1))
		if e != nil || len(value) > aigateway.MaxInputAssetBytes {
			return nil, errors.New("AI input exceeds one MiB")
		}
		return value, nil
	}
	if parent == "multipart/form-data" {
		boundary := params["boundary"]
		if boundary == "" {
			err = errors.New("multipart boundary required")
			return
		}
		reader := multipart.NewReader(io.LimitReader(r.Body, aigateway.MaxInputAssetBytes+(64<<10)), boundary)
		seen := map[string]bool{}
		for {
			part, e := reader.NextPart()
			if e == io.EOF {
				break
			}
			if e != nil {
				err = e
				return
			}
			field := part.FormName()
			if seen[field] || (field != "file" && field != "purpose") {
				part.Close()
				err = errors.New("unsupported upload field")
				return
			}
			seen[field] = true
			if field == "purpose" {
				value, e := io.ReadAll(io.LimitReader(part, 65))
				part.Close()
				if e != nil || len(value) > 64 {
					err = errors.New("invalid file purpose")
					return
				}
				purpose = string(value)
				continue
			}
			name = part.FileName()
			contentType = part.Header.Get("Content-Type")
			data, err = read(part)
			part.Close()
			if err != nil {
				return
			}
		}
	} else {
		name = r.Header.Get("X-Neon-Filename")
		contentType = parent
		data, err = read(r.Body)
	}
	if err != nil {
		return
	}
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(data)
	}
	if !inputFilenameValid(name) || (purpose != "user_data" && purpose != "vision") || aigateway.ValidateInputMedia(contentType, data) != nil || purpose == "vision" && aigateway.InputMediaKind(contentType) != "image" {
		err = errors.New("invalid file metadata or media")
	}
	return
}

func (s *server) aiInputFiles(w http.ResponseWriter, r *http.Request) {
	if accessFrom(r).Level < 2 {
		aiInputFail(w, r, 403, "forbidden", "AI input files require project editing access")
		return
	}
	s.serveAIInputFiles(w, r, userFrom(r).ID, r.PathValue("project"), r.PathValue("branch"))
}

func (s *server) aiPublicInputFiles(w http.ResponseWriter, r *http.Request) {
	header := aiAuthorization(r)
	if !strings.HasPrefix(header, "Bearer ") {
		aigateway.Error(w, 401, "invalid_backend_credential")
		return
	}
	branch := r.PathValue("branch")
	verified, err := s.verifyBackendCredential(r.Context(), strings.TrimPrefix(header, "Bearer "), branch, "ai_gateway:invoke", "")
	if err != nil {
		code := 503
		if errors.Is(err, errBackendCredentialInvalid) {
			code = 401
		}
		if errors.Is(err, errBackendCredentialForbidden) {
			code = 403
		}
		aigateway.Error(w, code, "credential_scope_denied")
		return
	}
	var actor, project string
	if err = s.db.QueryRow(r.Context(), `SELECT issuer_id,project_id FROM backend_credentials WHERE id=$1`, verified["credential_id"]).Scan(&actor, &project); err != nil {
		aigateway.Error(w, 503, "credential_unavailable")
		return
	}
	s.serveAIInputFiles(w, r, actor, project, branch)
}

func (s *server) serveAIInputFiles(w http.ResponseWriter, r *http.Request, actor, project, branch string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if s.ai == nil || s.backendKeys == nil {
		aiInputFail(w, r, 503, "ai_gateway_disabled", "AI input storage is not configured")
		return
	}
	var org string
	if err := s.db.QueryRow(r.Context(), `SELECT org_id FROM ai_input_file_scopes WHERE branch_id=$1 AND project_id=$2`, branch, project).Scan(&org); err != nil {
		if isNoRows(err) {
			aiInputFail(w, r, 404, "not_found", "AI input branch not found")
		} else {
			aiInputFailure(w, r, err)
		}
		return
	}
	id := r.PathValue("inputFile")
	if id != "" && !aiInputID.MatchString(id) {
		aiInputFail(w, r, 404, "not_found", "AI input file not found")
		return
	}
	if r.Method == http.MethodPost {
		name, contentType, purpose, data, err := readAIInputUpload(r)
		if err != nil {
			aiInputFail(w, r, 422, "invalid_input_file", "Use a supported file of at most one MiB and a safe filename")
			return
		}
		defer clear(data)
		ttl := int64(86400)
		if raw := r.Header.Get("X-Neon-File-TTL"); raw != "" {
			ttl, err = strconv.ParseInt(raw, 10, 64)
		}
		if err != nil || ttl < 60 || ttl > 604800 {
			aiInputFail(w, r, 422, "invalid_input_file_expiry", "File lifetime must be between 60 seconds and seven days")
			return
		}
		digest := sha256.Sum256(data)
		input := record{"filename": name, "content_type": contentType, "purpose": purpose, "sha256": hex.EncodeToString(digest[:]), "bytes": len(data), "ttl_seconds": ttl}
		s.aiInputMutation(w, r, actor, project, branch, input, func(tx pgx.Tx) (record, int, error) {
			if _, err := tx.Exec(r.Context(), `UPDATE ai_input_files SET ciphertext=NULL,deleted_at=clock_timestamp() WHERE branch_id=$1 AND deleted_at IS NULL AND expires_at<=clock_timestamp()`, branch); err != nil {
				return nil, 0, err
			}
			var count int
			var bytes int64
			if err := tx.QueryRow(r.Context(), `SELECT count(*),COALESCE(sum(bytes),0) FROM ai_input_files WHERE branch_id=$1 AND ciphertext IS NOT NULL`, branch).Scan(&count, &bytes); err != nil {
				return nil, 0, err
			}
			if count >= 32 || bytes+int64(len(data)) > 16<<20 {
				return nil, 0, providerProblem(429, "input_file_quota_exceeded", "At most 32 live files and 16 MiB are allowed per branch")
			}
			file := aiInputFile{ID: newID("nif_"), Project: project, Org: org, Branch: branch, Actor: actor, Filename: name, ContentType: contentType, Purpose: purpose}
			if err := s.aiInputSeal(&file, data); err != nil {
				return nil, 0, err
			}
			defer clear(file.Ciphertext)
			_, err := tx.Exec(r.Context(), `INSERT INTO ai_input_files(id,project_id,org_id,branch_id,actor_id,filename,content_type,purpose,bytes,sha256,key_version,ciphertext,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,now()+make_interval(secs=>$13))`, file.ID, project, org, branch, actor, name, contentType, purpose, file.Bytes, file.SHA256, file.KeyVersion, file.Ciphertext, ttl)
			if err != nil {
				return nil, 0, err
			}
			value, err := aiInputOne(r.Context(), tx, `SELECT `+aiInputPublicColumns+` FROM ai_input_files WHERE id=$1`, file.ID)
			return value, 201, err
		})
		return
	}
	if r.Method == http.MethodDelete {
		if id == "" {
			aiInputFail(w, r, 404, "not_found", "AI input file not found")
			return
		}
		s.aiInputMutation(w, r, actor, project, branch, record{"id": id}, func(tx pgx.Tx) (record, int, error) {
			var exists bool
			if err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM ai_input_files WHERE id=$1 AND project_id=$2 AND branch_id=$3)`, id, project, branch).Scan(&exists); err != nil {
				return nil, 0, err
			}
			if !exists {
				return nil, 0, providerProblem(404, "not_found", "AI input file not found")
			}
			_, err := tx.Exec(r.Context(), `UPDATE ai_input_files SET deleted_at=clock_timestamp(),ciphertext=NULL WHERE id=$1 AND deleted_at IS NULL`, id)
			return record{"id": id, "object": "file", "deleted": true}, 200, err
		})
		return
	}
	if id == "" {
		items, err := s.many(r.Context(), `SELECT `+aiInputPublicColumns+` FROM ai_input_files WHERE project_id=$1 AND branch_id=$2 AND deleted_at IS NULL AND expires_at>clock_timestamp() ORDER BY created_at DESC,id DESC LIMIT 32`, project, branch)
		if err != nil {
			aiInputFailure(w, r, err)
			return
		}
		jsonResponse(w, 200, record{"object": "list", "data": items, "has_more": false})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/content") {
		file, err := s.loadAIInput(r.Context(), id, project, branch)
		if err != nil {
			aiInputReadFailure(w, r, err)
			return
		}
		data, err := s.aiInputOpen(file)
		if err != nil {
			aiInputFailure(w, r, err)
			return
		}
		defer clear(data)
		w.Header().Set("Content-Type", file.ContentType)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file.Filename}))
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("ETag", `"`+file.SHA256+`"`)
		w.WriteHeader(200)
		_, _ = w.Write(data)
		return
	}
	value, err := s.one(r.Context(), `SELECT `+aiInputPublicColumns+` FROM ai_input_files WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND deleted_at IS NULL AND expires_at>clock_timestamp()`, id, project, branch)
	if err != nil {
		aiInputReadFailure(w, r, err)
		return
	}
	jsonResponse(w, 200, value)
}

func aiInputReadFailure(w http.ResponseWriter, r *http.Request, err error) {
	if isNoRows(err) {
		aiInputFail(w, r, 404, "input_file_not_found", "AI input file is unavailable in this branch")
	} else {
		aiInputFailure(w, r, err)
	}
}

func (s *server) loadAIInput(ctx context.Context, id, project, branch string) (aiInputFile, error) {
	var f aiInputFile
	err := s.db.QueryRow(ctx, `SELECT `+aiInputPrivateColumns+` FROM ai_input_files WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND deleted_at IS NULL AND expires_at>clock_timestamp()`, id, project, branch).Scan(&f.ID, &f.Project, &f.Org, &f.Branch, &f.Actor, &f.Filename, &f.ContentType, &f.Purpose, &f.SHA256, &f.KeyVersion, &f.Bytes, &f.Ciphertext)
	return f, err
}

func (s *server) expireAIInputs(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `WITH expired AS (SELECT id FROM ai_input_files WHERE deleted_at IS NULL AND expires_at<=clock_timestamp() ORDER BY expires_at,id LIMIT 100 FOR UPDATE SKIP LOCKED) UPDATE ai_input_files f SET deleted_at=clock_timestamp(),ciphertext=NULL FROM expired WHERE f.id=expired.id`)
	return err
}

// A caller may have already received decrypted bytes when it deletes a file.
// Accepted inference may finish; new admissions recheck every reference first.
func pinAIInputFiles(ctx context.Context, tx pgx.Tx, request, project, branch string, ids []string) error {
	if len(ids) > aigateway.MaxInputAssets {
		return providerProblem(422, "invalid_input_files", "Too many input files")
	}
	for _, id := range ids {
		var found string
		err := tx.QueryRow(ctx, `SELECT id FROM ai_input_files WHERE id=$1 AND project_id=$2 AND branch_id=$3 AND deleted_at IS NULL AND expires_at>clock_timestamp() FOR SHARE`, id, project, branch).Scan(&found)
		if isNoRows(err) {
			return providerProblem(422, "input_file_not_found", "Input file expired or was deleted before admission")
		}
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO ai_request_input_files(request_id,file_id,project_id,branch_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, request, id, project, branch); err != nil {
			return err
		}
	}
	return nil
}

func aiInputOne(ctx context.Context, tx pgx.Tx, query string, args ...any) (record, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	value, err := pgx.CollectOneRow(rows, pgx.RowToMap)
	return record(value), err
}

func (s *server) runAIInputExpiry(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		step, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.expireAIInputs(step)
		cancel()
		if err != nil && ctx.Err() == nil {
			s.logger.Warn("Expired AI input payload cleanup unavailable")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func aiInputFail(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	if strings.HasPrefix(r.URL.Path, "/ai/") {
		aigateway.Error(w, status, code)
		return
	}
	fail(w, r, status, code, message)
}
func aiInputFailure(w http.ResponseWriter, r *http.Request, err error) {
	var problem aiProviderProblem
	if errors.As(err, &problem) {
		aiInputFail(w, r, problem.status, problem.code, problem.message)
		return
	}
	aiInputFail(w, r, 503, "input_storage_unavailable", "AI input storage could not be completed")
}
