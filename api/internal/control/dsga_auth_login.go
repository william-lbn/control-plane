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
	"time"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	core "k8s.io/api/core/v1"
)

const dsgaAuthLoginPath = "/dsg/v1/catalog/service-login"

type dsgaAuthLoginRequest struct {
	ProtocolVersion uint32        `json:"protocol_version"`
	ProjectID       string        `json:"project_id"`
	BranchID        string        `json:"branch_id"`
	EndpointID      string        `json:"endpoint_id"`
	PGBootID        dsgapi.Uint64 `json:"pg_boot_id"`
	Database        string        `json:"database"`
	Generation      dsgapi.Uint64 `json:"generation"`
	Verifier        *string       `json:"verifier"`
}

type dsgaAuthLoginReceipt struct {
	ProtocolVersion uint32        `json:"protocol_version"`
	BranchID        string        `json:"branch_id"`
	PGBootID        dsgapi.Uint64 `json:"pg_boot_id"`
	Generation      dsgapi.Uint64 `json:"generation"`
	Enabled         bool          `json:"enabled"`
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
	if authorize == nil {
		return errDSGACreationIdentity
	}
	return s.applyDSGAAuthLogin(ctx, runtime, p, verifier, 0, func(ctx context.Context) error {
		latest, err := s.dsgaEndpointRuntime(ctx, p.ProjectID, p.EndpointID)
		if err != nil || latest == nil || latest.Operation != runtime.Operation || latest.VMUID != runtime.VMUID {
			return errDSGACreationIdentity
		}
		return authorize(ctx)
	})
}

// The caller proves which operation owns this generation. A resume can use
// its bound, initialized Writer before that Writer becomes the active endpoint.
func (s *server) applyDSGAAuthLogin(ctx context.Context, runtime *dsgaEndpointRuntime, p managedAuthPayload, verifier *string, boot dsgapi.Uint64, authorize func(context.Context) error) error {
	refusal := dataAPIPrerequisiteError{"auth_writer_service_login_unconfirmed", "Original DSGA Writer service login is unconfirmed"}
	if runtime == nil || runtime.Payload.ProjectID != p.ProjectID || runtime.Payload.BranchID != p.BranchID ||
		runtime.Payload.EndpointID != p.EndpointID || runtime.Payload.EndpointType != "read_write" ||
		p.Generation < 1 || authorize == nil {
		return refusal
	}
	_, vm, _, selected, err := s.readDSGABoundSelection(ctx, runtime)
	if err != nil || boot != 0 && selected.Instance.PGBootID != boot {
		return refusal
	}
	var uid string
	if err = s.db.QueryRow(ctx, `SELECT service_uid FROM dsga_creation_services
	 WHERE operation_id=$1 AND service_kind='management' AND vm_uid=$2`, runtime.Operation, runtime.VMUID).Scan(&uid); err != nil {
		return refusal
	}
	service := new(core.Service)
	wanted, err := renderDSGAService(vm, runtime.Payload, runtime.Plan, runtime.Operation, "management")
	if err != nil || managedSQLRead(s.kube, ctx, "service", vm.Name+"-management", service) != nil ||
		!sameDSGAService(service, wanted, uid) {
		return refusal
	}
	var credential core.Secret
	name := vm.Name + "-credentials"
	if managedSQLRead(s.kube, ctx, "secret", name, &credential) != nil {
		return refusal
	}
	source, err := snapshotDSGACredentials(&credential, runtime.Plan.Namespace, name, p.ProjectID, p.EndpointID)
	if err != nil || source.reference() != runtime.Plan.Credentials {
		return refusal
	}
	if err := authorize(ctx); err != nil {
		return refusal
	}
	input := dsgaAuthLoginRequest{1, p.ProjectID, p.BranchID, p.EndpointID, selected.Instance.PGBootID,
		p.Spec.Database, dsgapi.Uint64(p.Generation), verifier}
	key := ed25519.NewKeyFromSeed(credential.Data["controlSigningSeed"])
	client := newDSGAProxy(nil).execution
	client.Timeout = 35 * time.Second
	defer client.CloseIdleConnections()
	if err := requestDSGAAuthLogin(ctx, client, "http://"+net.JoinHostPort(service.Spec.ClusterIP, "3080"), key, runtime, input); err != nil {
		return refusal
	}
	return authorize(ctx)
}

func requestDSGAAuthLogin(ctx context.Context, client *http.Client, target string, key ed25519.PrivateKey, runtime *dsgaEndpointRuntime, input dsgaAuthLoginRequest) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return errors.New("cannot encode service login intent")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target+dsgaAuthLoginPath, bytes.NewReader(raw))
	if err != nil {
		return errors.New("cannot construct service login request")
	}
	req.Header.Set("Authorization", "Bearer "+dsgaAuthLoginJWT(key, runtime, raw, time.Now()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("service login response unavailable")
	}
	defer resp.Body.Close()
	var receipt dsgaAuthLoginReceipt
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(body) > 4096 || resp.StatusCode != http.StatusOK ||
		resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Cache-Control") != "no-store" ||
		dsgapi.UnmarshalObject(body, &receipt) != nil ||
		receipt != (dsgaAuthLoginReceipt{1, input.BranchID, input.PGBootID, input.Generation, input.Verifier != nil}) {
		return errors.New("service login receipt differs")
	}
	return nil
}
