package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

const inputFilePath = "/api/v1/projects/prj_provider/branches/br_provider/ai-gateway/files"

var inputFilePDF = []byte("%PDF-1.7\nExplicit synthetic input document, not a customer prompt\n%%EOF\n")

func inputFileRequest(f *providerFixture, method, path, key string, data []byte, contentType, filename string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: "provider_org_admin-session"})
	r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: "provider_org_admin-csrf"})
	r.Header.Set("X-CSRF-Token", "provider_org_admin-csrf")
	r.Header.Set("Idempotency-Key", key)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if filename != "" {
		r.Header.Set("X-Neon-Filename", filename)
	}
	w := httptest.NewRecorder()
	f.s.routes().ServeHTTP(w, r)
	return w
}
func inputFileUpload(t *testing.T, f *providerFixture, key string) string {
	t.Helper()
	w := inputFileRequest(f, "POST", inputFilePath, key, inputFilePDF, "application/pdf", "probe.pdf")
	if w.Code != 201 {
		t.Fatalf("upload status %d: %s", w.Code, w.Body.String())
	}
	var v record
	if json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatal("file receipt JSON")
	}
	return stringVal(v["id"])
}

func TestAIInputFilesUploadReplayAndEncryptedHistory(t *testing.T) {
	f := newProviderFixture(t)
	id := inputFileUpload(t, f, "original-file-upload")
	w := inputFileRequest(f, "POST", inputFilePath, "original-file-upload", inputFilePDF, "application/pdf", "probe.pdf")
	if w.Code != 201 || w.Header().Get("Idempotency-Replayed") != "true" || !strings.Contains(w.Body.String(), id) {
		t.Fatal("lost upload duplicated its receipt")
	}
	w = inputFileRequest(f, "POST", inputFilePath, "original-file-upload", append(append([]byte{}, inputFilePDF...), 'x'), "application/pdf", "probe.pdf")
	if w.Code != 409 {
		t.Fatal("same key changed content")
	}
	var count int
	var ciphertext []byte
	if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM ai_input_files`).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate file row", count, err)
	}
	if err := f.s.db.QueryRow(f.ctx, `SELECT ciphertext FROM ai_input_files WHERE id=$1`, id).Scan(&ciphertext); err != nil || bytes.Contains(ciphertext, inputFilePDF) || len(ciphertext) != len(inputFilePDF)+28 {
		t.Fatal("input was not a scoped encrypted envelope", err)
	}
	w = inputFileRequest(f, "GET", inputFilePath+"/"+id+"/content", "", nil, "", "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), inputFilePDF) || w.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("protected content response changed bytes or cache policy")
	}
	w = inputFileRequest(f, "DELETE", inputFilePath+"/"+id, "original-file-delete", nil, "", "")
	if w.Code != 200 {
		t.Fatal("file delete failed")
	}
	w = inputFileRequest(f, "DELETE", inputFilePath+"/"+id, "original-file-delete", nil, "", "")
	if w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("file deletion could not replay")
	}
	var retained, erased bool
	if err := f.s.db.QueryRow(f.ctx, `SELECT deleted_at IS NOT NULL,ciphertext IS NULL FROM ai_input_files WHERE id=$1`, id).Scan(&retained, &erased); err != nil || !retained || !erased {
		t.Fatal("delete lost metadata or retained encrypted content", err)
	}
	if _, err := f.s.db.Exec(f.ctx, `DELETE FROM ai_input_files WHERE id=$1`, id); err == nil {
		t.Fatal("input metadata history could be removed")
	}
	if _, err := f.s.db.Exec(f.ctx, `UPDATE ai_input_mutations SET response='{}'`); err == nil {
		t.Fatal("input replay history mutable")
	}
	if inputFileRequest(f, "GET", inputFilePath+"/"+id, "", nil, "", "").Code != 404 {
		t.Fatal("deleted input still available")
	}
}

func TestAIInputFilesCipherAndBranchIsolation(t *testing.T) {
	f := newProviderFixture(t)
	id := inputFileUpload(t, f, "scope-original-upload")
	original, err := f.s.loadAIInput(f.ctx, id, "prj_provider", "br_provider")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"project", "org", "branch", "actor", "filename", "type", "sha"} {
		copy := original
		switch field {
		case "project":
			copy.Project += "x"
		case "org":
			copy.Org += "x"
		case "branch":
			copy.Branch += "x"
		case "actor":
			copy.Actor += "x"
		case "filename":
			copy.Filename += "x"
		case "type":
			copy.ContentType = "image/png"
		case "sha":
			copy.SHA256 = strings.Repeat("0", 64)
		}
		if _, err = f.s.aiInputOpen(copy); err == nil {
			t.Fatal("cipher envelope could move scope", field)
		}
	}
	if _, err = f.s.db.Exec(f.ctx, `UPDATE ai_input_files SET branch_id='br_moved' WHERE id=$1`, id); err == nil {
		t.Fatal("input identity mutable")
	}
	f.exec(`INSERT INTO branches(id,project_id,name,parent_branch_id,state,created_at) VALUES('br_input_child','prj_provider','input-child','br_provider','ready',now())`)
	other := strings.Replace(inputFilePath, "br_provider", "br_input_child", 1)
	if inputFileRequest(f, "GET", other+"/"+id+"/content", "", nil, "", "").Code != 404 {
		t.Fatal("child branch read its parent's private input")
	}
	raw := []byte(`{"model":"static-route","input":[{"type":"input_file","file_id":"` + id + `"}]}`)
	if _, _, err = f.s.resolveAIInputs(f.ctx, "prj_provider", "br_input_child", raw, aigateway.Responses); err == nil {
		t.Fatal("parent handle could be used in child inference")
	}
	f.exec(`UPDATE organization_members SET role='viewer' WHERE user_id='provider_org_admin'`)
	if inputFileRequest(f, "GET", inputFilePath, "", nil, "", "").Code != 403 {
		t.Fatal("viewer could access staged model inputs")
	}
}

func TestAIInputFilesSDKMultipartAndRevocation(t *testing.T) {
	f := newProviderFixture(t)
	spec := backendCredentialSpec{Name: "file-sdk", Scopes: []string{"ai_gateway:invoke"}, BranchScope: "self_and_descendants", ExpiresAt: time.Now().UTC().Add(time.Hour)}
	issued := f.call("provider_org_admin", "POST", "/api/v1/projects/prj_provider/branches/br_provider/credentials", "input-sdk-credential", spec, 201)
	token := stringVal(issued["api_token"])
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("purpose", "user_data"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", "SDK-input.pdf")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(inputFilePDF)
	writer.Close()
	r := httptest.NewRequest("POST", "/ai/v1/br_provider/v1/files", bytes.NewReader(body.Bytes()))
	r.SetPathValue("branch", "br_provider")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Idempotency-Key", "sdk-original-file-upload")
	r.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	f.s.aiPublicInputFiles(w, r)
	if w.Code != 201 {
		t.Fatalf("SDK multipart status %d %s", w.Code, w.Body.String())
	}
	var value record
	json.Unmarshal(w.Body.Bytes(), &value)
	id := stringVal(value["id"])
	if !aiInputID.MatchString(id) || value["content_type"] != "application/pdf" || value["filename"] != "SDK-input.pdf" {
		t.Fatal("SDK file object did not reflect uploaded bytes")
	}
	f.exec(`UPDATE backend_credentials SET revoked_at=now() WHERE id=$1`, issued["credential"].(map[string]any)["id"])
	r = httptest.NewRequest("GET", "/ai/v1/br_provider/v1/files/"+id, nil)
	r.SetPathValue("branch", "br_provider")
	r.SetPathValue("inputFile", id)
	r.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	f.s.aiPublicInputFiles(w, r)
	if w.Code != 401 {
		t.Fatal("revoked SDK key could retrieve file metadata")
	}
}

func TestAIInputFilesNativeHTTPAndImmutableReferences(t *testing.T) {
	f := newProviderFixture(t)
	id := inputFileUpload(t, f, "native-shared-file")
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("native JSON")
		}
		raw, _ := json.Marshal(body)
		if bytes.Contains(raw, []byte(id)) || !bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(inputFilePDF))) {
			t.Error("gateway handle escaped or native file bytes changed")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/responses":
			fmt.Fprint(w, `{"object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"asset accepted"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`)
		case "/v1/messages":
			fmt.Fprint(w, `{"type":"message","content":[{"type":"text","text":"asset accepted"}],"usage":{"input_tokens":3,"output_tokens":2}}`)
		case "/v1beta/models/upstream:generateContent":
			fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"asset accepted"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`)
		default:
			t.Error("wrong native path")
			w.WriteHeader(404)
		}
	}))
	defer peer.Close()
	route := f.s.ai.Routes[0]
	route.BaseURL = peer.URL + "/v1"
	route.UpstreamModel = "upstream"
	route.InputModalities = []string{"text", "image", "pdf"}
	route.Protocols = []string{aigateway.Responses, aigateway.Messages, aigateway.Gemini}
	f.s.ai.Routes = []aigateway.Route{route}
	for _, protocol := range route.Protocols {
		var body record
		switch protocol {
		case aigateway.Responses:
			body = record{"model": "static-route", "input": []any{record{"role": "user", "content": []any{record{"type": "input_file", "file_id": id}}}}, "max_output_tokens": 8}
		case aigateway.Messages:
			body = record{"model": "static-route", "messages": []any{record{"role": "user", "content": []any{record{"type": "document", "source": record{"type": "file", "file_id": id}}}}}, "max_tokens": 8}
		case aigateway.Gemini:
			body = record{"model": "static-route", "contents": []any{record{"role": "user", "parts": []any{record{"fileData": record{"fileUri": "neon-file:" + id, "mimeType": "application/pdf"}}}}}, "generationConfig": record{"maxOutputTokens": 8}}
		}
		raw, _ := json.Marshal(body)
		snapshot, err := f.s.aiBranchCatalog(f.ctx, false, "br_provider")
		if err != nil {
			t.Fatal(err)
		}
		ticket := f.s.signAIProtocolTicket("provider_org_admin", "br_provider", "static-route", raw, snapshot.Config.Generation, protocol)
		r := httptest.NewRequest("POST", "/ai/v1/br_provider/native", bytes.NewReader(raw))
		r.SetPathValue("branch", "br_provider")
		r.SetPathValue("protocol", protocol)
		r.Header.Set("X-Neon-Console-Ticket", ticket)
		w := httptest.NewRecorder()
		f.s.aiPublic(w, r)
		if w.Code != 200 {
			t.Fatalf("%s native input status %d %s", protocol, w.Code, w.Body.String())
		}
	}
	var requests, refs int
	if err := f.s.db.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM ai_gateway_requests WHERE state='completed' AND total_tokens=5),(SELECT count(*) FROM ai_request_input_files WHERE file_id=$1)`, id).Scan(&requests, &refs); err != nil || requests != 3 || refs != 3 || calls.Load() != 3 {
		t.Fatal("native usage/reference accounting", requests, refs, calls.Load(), err)
	}
	if _, err := f.s.db.Exec(f.ctx, `DELETE FROM ai_request_input_files`); err == nil {
		t.Fatal("admitted input receipts mutable")
	}
}

func TestAIInputFilesDeleteAdmissionAndAcceptedSettlement(t *testing.T) {
	f := creditFixture(t, 1000)
	id := inputFileUpload(t, f, "deleted-before-admission")
	raw := []byte(`{"model":"static-route","input":[{"type":"input_file","file_id":"` + id + `"}]}`)
	if _, _, err := f.s.resolveAIInputs(f.ctx, "prj_provider", "br_provider", raw, aigateway.Responses); err != nil {
		t.Fatal(err)
	}
	if inputFileRequest(f, "DELETE", inputFilePath+"/"+id, "delete-after-resolve", nil, "", "").Code != 200 {
		t.Fatal("delete")
	}
	request := newID("air_")
	if _, err := f.s.reserveAIRequestFor(f.ctx, request, "prj_provider", "br_provider", "provider_org_admin", "", aigateway.ChatRequest{Model: "static-route", MaxTokens: 8, InputFiles: []string{id}}, f.s.ai); err == nil {
		t.Fatal("deleted reference admitted after resolution")
	}
	var n int
	if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM ai_gateway_requests WHERE id=$1`, request).Scan(&n); err != nil || n != 0 {
		t.Fatal("failed reference left partial request", err)
	}
	creditCounters(t, f, 1000, 0, 0)
	accepted := inputFileUpload(t, f, "accepted-before-deletion")
	request = newID("air_")
	if ok, err := f.s.reserveAIRequestFor(f.ctx, request, "prj_provider", "br_provider", "provider_org_admin", "", aigateway.ChatRequest{Model: "static-route", MaxTokens: 8, InputFiles: []string{accepted}}, f.s.ai); err != nil || !ok {
		t.Fatal("live file admission", err)
	}
	inputFileRequest(f, "DELETE", inputFilePath+"/"+accepted, "delete-already-accepted", nil, "", "")
	if err := f.s.settleAIRequest(f.ctx, request, "br_provider", aigateway.Outcome{Status: "completed", Dispatched: true, Usage: &aigateway.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	creditCounters(t, f, 1000, 7, 0)
	if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM ai_request_input_files WHERE request_id=$1 AND file_id=$2`, request, accepted).Scan(&n); err != nil || n != 1 {
		t.Fatal("accepted file attribution erased", err)
	}
}

func TestAIInputFilesExpiryAndMissingKeyFailClosed(t *testing.T) {
	f := newProviderFixture(t)
	live := inputFileUpload(t, f, "live-file-preserved")
	file := aiInputFile{ID: newID("nif_"), Project: "prj_provider", Org: "org_provider", Branch: "br_provider", Actor: "provider_org_admin", Filename: "expired.pdf", ContentType: "application/pdf", Purpose: "user_data"}
	if err := f.s.aiInputSeal(&file, inputFilePDF); err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO ai_input_files(id,project_id,org_id,branch_id,actor_id,filename,content_type,purpose,bytes,sha256,key_version,ciphertext,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,now()-interval '2 hours',now()-interval '1 hour')`, file.ID, file.Project, file.Org, file.Branch, file.Actor, file.Filename, file.ContentType, file.Purpose, file.Bytes, file.SHA256, file.KeyVersion, file.Ciphertext)
	if inputFileRequest(f, "GET", inputFilePath+"/"+file.ID+"/content", "", nil, "", "").Code != 404 {
		t.Fatal("expired file readable before cleanup")
	}
	if err := f.s.expireAIInputs(f.ctx); err != nil {
		t.Fatal(err)
	}
	var erased bool
	if err := f.s.db.QueryRow(f.ctx, `SELECT ciphertext IS NULL AND deleted_at IS NOT NULL FROM ai_input_files WHERE id=$1`, file.ID).Scan(&erased); err != nil || !erased {
		t.Fatal("expired payload not erased", err)
	}
	if inputFileRequest(f, "GET", inputFilePath+"/"+live+"/content", "", nil, "", "").Code != 200 {
		t.Fatal("expiry removed live input")
	}
	delete(f.s.backendKeys.decoded, "v1")
	if inputFileRequest(f, "GET", inputFilePath+"/"+live+"/content", "", nil, "", "").Code != 503 {
		t.Fatal("missing decryption key exposed or guessed input")
	}
}

func TestAIInputFilesCapacityAndConcurrentLastSlot(t *testing.T) {
	f := newProviderFixture(t)
	for i := 0; i < 31; i++ {
		inputFileUpload(t, f, fmt.Sprintf("capacity-input-%02d", i))
	}
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			codes <- inputFileRequest(f, "POST", inputFilePath, fmt.Sprintf("concurrent-last-slot-%d", i), inputFilePDF, "application/pdf", "probe.pdf").Code
		}(i)
	}
	a, b := <-codes, <-codes
	if !((a == 201 && b == 429) || (a == 429 && b == 201)) {
		t.Fatal("concurrent branch file quota bypassed", a, b)
	}
	// A separate disposable branch tests the byte counter without a 16 MiB
	// physical I/O campaign. These intentionally unopened fixture envelopes are
	// compressible; public admission must refuse their fully occupied byte quota.
	f.exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('br_input_bytes','prj_provider','input-bytes','ready',now())`)
	for i := 0; i < 16; i++ {
		f.exec(`INSERT INTO ai_input_files(id,project_id,org_id,branch_id,actor_id,filename,content_type,purpose,bytes,sha256,key_version,ciphertext,expires_at) VALUES($1,'prj_provider','org_provider','br_input_bytes','provider_org_admin','quota.pdf','application/pdf','user_data',1048576,$2,'v1',decode(repeat('00',1048604),'hex'),now()+interval '1 hour')`, fmt.Sprintf("nif_%024x", i), strings.Repeat("a", 64))
	}
	target := strings.Replace(inputFilePath, "br_provider", "br_input_bytes", 1)
	if inputFileRequest(f, "POST", target, "byte-counter-capacity", inputFilePDF, "application/pdf", "probe.pdf").Code != 429 {
		t.Fatal("byte quota bypassed below file-count limit")
	}
}

func TestAIInputFilesRestrictedGatewayAndUnauthenticatedCatalog(t *testing.T) {
	f := newProviderFixture(t)
	var schema string
	if err := f.s.db.QueryRow(f.ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := newID("inputread_")
	f.exec("CREATE ROLE " + role + " LOGIN PASSWORD 'input-ci-disposable'")
	f.exec("GRANT USAGE ON SCHEMA " + schema + " TO " + role)
	f.exec("GRANT SELECT(id,project_id) ON branches TO " + role)
	f.exec("GRANT SELECT(id,org_id) ON projects TO " + role)
	f.exec("GRANT SELECT,INSERT ON ai_input_files,ai_input_mutations,ai_request_input_files TO " + role)
	f.exec("GRANT SELECT ON ai_input_file_scopes TO " + role)
	f.exec("GRANT UPDATE(deleted_at,ciphertext) ON ai_input_files TO " + role)
	cfg, err := pgxpool.ParseConfig(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = role
	cfg.ConnConfig.Password = "input-ci-disposable"
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	limited := &server{db: db, ai: f.s.ai, backendKeys: f.s.backendKeys}
	r := httptest.NewRequest("POST", inputFilePath, bytes.NewReader(inputFilePDF))
	r.Header.Set("Content-Type", "application/pdf")
	r.Header.Set("X-Neon-Filename", "limited.pdf")
	r.Header.Set("Idempotency-Key", "limited-input-original")
	w := httptest.NewRecorder()
	limited.serveAIInputFiles(w, r, "provider_org_admin", "prj_provider", "br_provider")
	if w.Code != 201 {
		t.Fatalf("restricted input role needs excess privileges: %d %s", w.Code, w.Body.String())
	}
	if _, err = db.Exec(f.ctx, `UPDATE ai_input_files SET org_id='moved'`); err == nil {
		t.Fatal("Gateway can move input ownership")
	}
	if _, err = db.Exec(f.ctx, `UPDATE ai_credit_accounts SET credited_micros=credited_micros+1`); err == nil {
		t.Fatal("input grants permit credit issuance")
	}
	// Missing credentials must fail before catalog access, even if catalog
	// storage/keys are unavailable. This also prevents alias existence probes.
	offline := &server{backendKeys: f.s.backendKeys}
	r = httptest.NewRequest("POST", "/ai/v1/br_provider/v1/chat/completions", strings.NewReader(`{"model":"private-alias","messages":[{"role":"user","content":"probe"}]}`))
	r.SetPathValue("branch", "br_provider")
	w = httptest.NewRecorder()
	offline.aiPublic(w, r)
	if w.Code != 401 {
		t.Fatal("anonymous call touched private catalog before authentication", w.Code)
	}
}

func TestAIInputFilesAudioReferenceAndExpiryWithoutKeys(t *testing.T) {
	f := newProviderFixture(t)
	// A minimal PCM WAV header plus two samples; the gateway validates its media
	// signature and native bytes, not transcription quality.
	wav := []byte{'R', 'I', 'F', 'F', 40, 0, 0, 0, 'W', 'A', 'V', 'E', 'f', 'm', 't', ' ', 16, 0, 0, 0, 1, 0, 1, 0, 0x40, 0x1f, 0, 0, 0x80, 0x3e, 0, 0, 2, 0, 16, 0, 'd', 'a', 't', 'a', 4, 0, 0, 0, 0, 0, 0, 0}
	upload := inputFileRequest(f, "POST", inputFilePath, "audio-reference-original", wav, "audio/wav", "probe.wav")
	if upload.Code != 201 {
		t.Fatalf("audio upload %d %s", upload.Code, upload.Body.String())
	}
	var metadata record
	json.Unmarshal(upload.Body.Bytes(), &metadata)
	id := stringVal(metadata["id"])
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		messages := body["messages"].([]any)
		content := messages[0].(map[string]any)["content"].([]any)
		audio := content[0].(map[string]any)["input_audio"].(map[string]any)
		data, err := base64.StdEncoding.DecodeString(audio["data"].(string))
		if err != nil || !bytes.Equal(data, wav) || audio["format"] != "wav" || audio["file_id"] != nil {
			t.Error("Chat audio did not contain exact native bytes")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"chat.completion","choices":[{"message":{"role":"assistant","content":"audio bytes accepted"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	defer peer.Close()
	route := f.s.ai.Routes[0]
	route.BaseURL = peer.URL + "/v1"
	route.InputModalities = []string{"text", "audio"}
	f.s.ai.Routes = []aigateway.Route{route}
	raw := []byte(`{"model":"static-route","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"file_id":"` + id + `"}}]}],"max_tokens":8}`)
	snapshot, err := f.s.aiBranchCatalog(f.ctx, false, "br_provider")
	if err != nil {
		t.Fatal(err)
	}
	ticket := f.s.signAITicketFor("provider_org_admin", "br_provider", "static-route", raw, snapshot.Config.Generation)
	r := httptest.NewRequest("POST", "/ai/v1/br_provider/v1/chat/completions", bytes.NewReader(raw))
	r.SetPathValue("branch", "br_provider")
	r.Header.Set("X-Neon-Console-Ticket", ticket)
	w := httptest.NewRecorder()
	f.s.aiPublic(w, r)
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("audio reference status %d %s", w.Code, w.Body.String())
	}
	expired := aiInputFile{ID: newID("nif_"), Project: "prj_provider", Org: "org_provider", Branch: "br_provider", Actor: "provider_org_admin", Filename: "expired.wav", ContentType: "audio/wav", Purpose: "user_data"}
	if err = f.s.aiInputSeal(&expired, wav); err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO ai_input_files(id,project_id,org_id,branch_id,actor_id,filename,content_type,purpose,bytes,sha256,key_version,ciphertext,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,now()-interval '2 hours',now()-interval '1 hour')`, expired.ID, expired.Project, expired.Org, expired.Branch, expired.Actor, expired.Filename, expired.ContentType, expired.Purpose, expired.Bytes, expired.SHA256, expired.KeyVersion, expired.Ciphertext)
	worker := &server{db: f.s.db, logger: f.s.logger}
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); worker.runAIInputExpiry(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	erased := false
	for time.Now().Before(deadline) {
		if err = f.s.db.QueryRow(f.ctx, `SELECT ciphertext IS NULL AND deleted_at IS NOT NULL FROM ai_input_files WHERE id=$1`, expired.ID).Scan(&erased); err != nil {
			t.Fatal(err)
		}
		if erased {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expiry controller did not stop")
	}
	if !erased {
		t.Fatal("expired content survived disabled inference/key loading")
	}
	if inputFileRequest(f, "GET", inputFilePath+"/"+id+"/content", "", nil, "", "").Code != 200 {
		t.Fatal("keyless expiry affected a live file")
	}
}
