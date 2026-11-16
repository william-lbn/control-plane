package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	core "k8s.io/api/core/v1"
)

const dsgaAuthLoginPath = "/dsg/v1/catalog/service-login"
const dsgaDataAPIServiceLogin = "data_api"
const dsgaFunctionServiceLogin = "function"
const dsgaRealtimeServiceLogin = "realtime"
const dsgaCatalogRoleServiceLogin = "catalog_role"

var dsgaRealtimeLoginID = regexp.MustCompile(`^rt[sq]_[a-f0-9]{24}$`)
var dsgaRealtimeRole = regexp.MustCompile(`^rt[pr]_[a-f0-9]{24}$`)

func realtimeRole(id string) string {
	if !dsgaRealtimeLoginID.MatchString(id) {
		return ""
	}
	if strings.HasPrefix(id, "rts_") {
		return "rtp_" + id[4:]
	}
	return "rtr_" + id[4:]
}

type dsgaRealtimeRelation struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

type dsgaAuthLoginRequest struct {
	ProtocolVersion uint32                  `json:"protocol_version"`
	Service         string                  `json:"service,omitempty"`
	ProjectID       string                  `json:"project_id"`
	BranchID        string                  `json:"branch_id"`
	EndpointID      string                  `json:"endpoint_id"`
	PGBootID        dsgapi.Uint64           `json:"pg_boot_id"`
	Database        string                  `json:"database"`
	FunctionID      string                  `json:"function_id,omitempty"`
	Schema          string                  `json:"schema,omitempty"`
	RealtimeID      string                  `json:"realtime_id,omitempty"`
	Relations       *[]dsgaRealtimeRelation `json:"relations,omitempty"`
	CatalogRole     *dsgaCatalogRoleIntent  `json:"catalog_role,omitempty"`
	Generation      dsgapi.Uint64           `json:"generation"`
	Verifier        *string                 `json:"verifier"`
}

type dsgaAuthLoginReceipt struct {
	ProtocolVersion uint32                  `json:"protocol_version"`
	Service         string                  `json:"service,omitempty"`
	BranchID        string                  `json:"branch_id"`
	PGBootID        dsgapi.Uint64           `json:"pg_boot_id"`
	FunctionID      string                  `json:"function_id,omitempty"`
	Schema          string                  `json:"schema,omitempty"`
	RealtimeID      string                  `json:"realtime_id,omitempty"`
	CatalogRole     *dsgaCatalogRoleReceipt `json:"catalog_role,omitempty"`
	Generation      dsgapi.Uint64           `json:"generation"`
	Enabled         bool                    `json:"enabled"`
}

func dsgaAuthLoginJWT(key ed25519.PrivateKey, runtime *dsgaEndpointRuntime, raw []byte, now time.Time) string {
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": computeKeyID(key)})
	claims, _ := json.Marshal(dsgaProxyClaims{
		Audience: "neon-dsga", Scope: "compute_ctl:dsga:service-login",
		ComputeID: runtime.binding().Name, VMUID: runtime.VMUID, ComputeInstanceID: runtime.Plan.ComputeInstanceID,
		IssuerEpoch: 1, NotBefore: uint64(now.Add(-time.Second).Unix()), Expires: uint64(now.Add(30 * time.Second).Unix()),
		Request: &dsgaArrayRequestBinding{ProtocolVersion: 1, Method: http.MethodPost,
			Path: dsgaAuthLoginPath, BodySHA256: managedSQLDigest(raw)},
	})
	message := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	return message + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(message)))
}

// Resolve the original Writer generation and immutable internal Service before
// authorizing one exact request. No credential or authorization is persisted
// in the public operation result.
func (s *server) reconcileDSGAAuthLogin(ctx context.Context, runtime *dsgaEndpointRuntime, p managedAuthPayload, verifier *string, authorize func(context.Context) error) error {
	return s.reconcileDSGAServiceLogin(ctx, runtime, authLoginRequest(p, verifier), authorize)
}

func authLoginRequest(p managedAuthPayload, verifier *string) dsgaAuthLoginRequest {
	return dsgaAuthLoginRequest{ProtocolVersion: 1, ProjectID: p.ProjectID, BranchID: p.BranchID,
		EndpointID: p.EndpointID, Database: p.Spec.Database, Generation: dsgapi.Uint64(p.Generation), Verifier: verifier}
}

func dataAPILoginRequest(p dataAPIPayload, verifier *string) dsgaAuthLoginRequest {
	return dsgaAuthLoginRequest{ProtocolVersion: 1, Service: dsgaDataAPIServiceLogin, ProjectID: p.ProjectID, BranchID: p.BranchID,
		EndpointID: p.EndpointID, Database: p.Spec.Database, Generation: dsgapi.Uint64(p.Generation), Verifier: verifier}
}

func functionServiceLoginRequest(ref functionSQLReference, generation int64, verifier *string) dsgaAuthLoginRequest {
	return dsgaAuthLoginRequest{ProtocolVersion: 1, Service: dsgaFunctionServiceLogin, ProjectID: ref.Scope.ProjectID,
		BranchID: ref.Scope.BranchID, EndpointID: ref.WriterID, Database: ref.Scope.Database,
		FunctionID: ref.Scope.FunctionID, Schema: ref.Scope.Schema, Generation: dsgapi.Uint64(generation), Verifier: verifier}
}

func (s *server) reconcileDSGAServiceLogin(ctx context.Context, runtime *dsgaEndpointRuntime, input dsgaAuthLoginRequest, authorize func(context.Context) error) error {
	if authorize == nil {
		return errDSGACreationIdentity
	}
	return s.applyDSGAServiceLogin(ctx, runtime, input, func(ctx context.Context) error {
		latest, err := s.dsgaEndpointRuntime(ctx, input.ProjectID, input.EndpointID)
		if err != nil || latest == nil || latest.Operation != runtime.Operation || latest.VMUID != runtime.VMUID {
			return errDSGACreationIdentity
		}
		return authorize(ctx)
	})
}

func (s *server) applyDSGAAuthLogin(ctx context.Context, runtime *dsgaEndpointRuntime, p managedAuthPayload, verifier *string, boot dsgapi.Uint64, authorize func(context.Context) error) error {
	input := authLoginRequest(p, verifier)
	input.PGBootID = boot
	return s.applyDSGAServiceLogin(ctx, runtime, input, authorize)
}

// A resume may use its own bound, initialized Writer before it becomes active.
func (s *server) applyDSGAServiceLogin(ctx context.Context, runtime *dsgaEndpointRuntime, input dsgaAuthLoginRequest, authorize func(context.Context) error) error {
	_, err := s.applyDSGAServiceLoginResult(ctx, runtime, input, authorize)
	return err
}

func (s *server) applyDSGAServiceLoginResult(ctx context.Context, runtime *dsgaEndpointRuntime, input dsgaAuthLoginRequest, authorize func(context.Context) error) (dsgaAuthLoginReceipt, error) {
	var zero dsgaAuthLoginReceipt
	refusal := dataAPIPrerequisiteError{"auth_writer_service_login_unconfirmed", "Original DSGA Writer service login is unconfirmed"}
	if (input.Service == dsgaCatalogRoleServiceLogin) != (input.CatalogRole != nil) {
		return zero, errDSGACreationIdentity
	}
	switch input.Service {
	case dsgaDataAPIServiceLogin:
		refusal = dataAPIPrerequisiteError{"data_api_writer_service_login_unconfirmed", "Original DSGA Writer Data API login is unconfirmed"}
	case dsgaFunctionServiceLogin:
		refusal = dataAPIPrerequisiteError{"function_writer_service_login_unconfirmed", "Original DSGA Writer Function login is unconfirmed"}
		if !functionDefinitionID.MatchString(input.FunctionID) || !validPGIdentifier(input.Schema) {
			return zero, refusal
		}
	case dsgaRealtimeServiceLogin:
		refusal = dataAPIPrerequisiteError{"realtime_writer_service_login_unconfirmed", "Original DSGA Writer Realtime login is unconfirmed"}
		if !dsgaRealtimeLoginID.MatchString(input.RealtimeID) || input.Relations == nil ||
			input.FunctionID != "" || input.Schema != "" {
			return zero, refusal
		}
		tables := *input.Relations
		if (strings.HasPrefix(input.RealtimeID, "rts_") && len(tables) != 0) ||
			(strings.HasPrefix(input.RealtimeID, "rtq_") && (len(tables) == 0 || len(tables) > 32)) {
			return zero, refusal
		}
		for _, relation := range tables {
			if !validPGIdentifier(relation.Schema) || !validPGIdentifier(relation.Name) {
				return zero, refusal
			}
		}
	case dsgaCatalogRoleServiceLogin:
		refusal = dataAPIPrerequisiteError{"catalog_writer_role_unconfirmed", "Original DSGA Writer catalog role and OID are unconfirmed"}
		if !input.CatalogRole.valid(input.Verifier) || input.Database != "postgres" || input.RealtimeID != "" ||
			input.FunctionID != "" || input.Schema != "" || input.Relations != nil {
			return zero, refusal
		}
	case "":
	default:
		return zero, errDSGACreationIdentity
	}
	if runtime == nil || runtime.Payload.ProjectID != input.ProjectID || runtime.Payload.BranchID != input.BranchID ||
		runtime.Payload.EndpointID != input.EndpointID || runtime.Payload.EndpointType != "read_write" ||
		input.Generation < 1 || uint64(input.Generation) > uint64(1<<63-1) || !validPGIdentifier(input.Database) || authorize == nil {
		return zero, refusal
	}
	_, vm, _, selected, err := s.readDSGABoundSelection(ctx, runtime)
	if err != nil || input.PGBootID != 0 && selected.Instance.PGBootID != input.PGBootID {
		return zero, refusal
	}
	var uid string
	if err = s.db.QueryRow(ctx, `SELECT service_uid FROM dsga_creation_services
	 WHERE operation_id=$1 AND service_kind='management' AND vm_uid=$2`, runtime.Operation, runtime.VMUID).Scan(&uid); err != nil {
		return zero, refusal
	}
	service := new(core.Service)
	wanted, err := renderDSGAService(vm, runtime.Payload, runtime.Plan, runtime.Operation, "management")
	if err != nil || managedSQLRead(s.kube, ctx, "service", vm.Name+"-management", service) != nil ||
		!sameDSGAService(service, wanted, uid) {
		return zero, refusal
	}
	var credential core.Secret
	name := vm.Name + "-credentials"
	if managedSQLRead(s.kube, ctx, "secret", name, &credential) != nil {
		return zero, refusal
	}
	source, err := snapshotDSGACredentials(&credential, runtime.Plan.Namespace, name, input.ProjectID, input.EndpointID)
	if err != nil || source.reference() != runtime.Plan.Credentials {
		return zero, refusal
	}
	if err := authorize(ctx); err != nil {
		return zero, refusal
	}
	input.PGBootID = selected.Instance.PGBootID
	key := ed25519.NewKeyFromSeed(credential.Data["controlSigningSeed"])
	client := newDSGAProxy(nil).execution
	client.Timeout = 35 * time.Second
	defer client.CloseIdleConnections()
	receipt, err := requestDSGAAuthLogin(ctx, client, "http://"+net.JoinHostPort(service.Spec.ClusterIP, "3080"), key, runtime, input)
	if err != nil {
		return zero, refusal
	}
	if err := authorize(ctx); err != nil {
		return zero, err
	}
	return receipt, nil
}

func requestDSGAAuthLogin(ctx context.Context, client *http.Client, target string, key ed25519.PrivateKey, runtime *dsgaEndpointRuntime, input dsgaAuthLoginRequest) (dsgaAuthLoginReceipt, error) {
	var zero dsgaAuthLoginReceipt
	raw, err := json.Marshal(input)
	if err != nil {
		return zero, errors.New("cannot encode service login intent")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target+dsgaAuthLoginPath, bytes.NewReader(raw))
	if err != nil {
		return zero, errors.New("cannot construct service login request")
	}
	req.Header.Set("Authorization", "Bearer "+dsgaAuthLoginJWT(key, runtime, raw, time.Now()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return zero, errors.New("service login response unavailable")
	}
	defer resp.Body.Close()
	var receipt dsgaAuthLoginReceipt
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(body) > 4096 || resp.StatusCode != http.StatusOK ||
		resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Cache-Control") != "no-store" ||
		dsgapi.UnmarshalObject(body, &receipt) != nil || !sameDSGAServiceLoginReceipt(input, receipt) {
		return zero, errors.New("service login receipt differs")
	}
	return receipt, nil
}

func sameDSGAServiceLoginReceipt(input dsgaAuthLoginRequest, receipt dsgaAuthLoginReceipt) bool {
	role := receipt.CatalogRole
	receipt.CatalogRole = nil
	if receipt != (dsgaAuthLoginReceipt{ProtocolVersion: 1, Service: input.Service, BranchID: input.BranchID, PGBootID: input.PGBootID,
		FunctionID: input.FunctionID, Schema: input.Schema, RealtimeID: input.RealtimeID,
		Generation: input.Generation, Enabled: input.Verifier != nil}) {
		return false
	}
	intent := input.CatalogRole
	if intent == nil {
		return role == nil
	}
	return role != nil && role.OperationID == intent.OperationID && role.Action == intent.Action && role.OID != 0 &&
		(intent.ExpectedOID == 0 || role.OID == intent.ExpectedOID)
}
