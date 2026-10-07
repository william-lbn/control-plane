package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type receipt struct {
	Hash       string         `json:"hash"`
	Payload    map[string]any `json:"payload"`
	ReceivedAt float64        `json:"received_at"`
	HashFormat string         `json:"hash_format,omitempty"`
}
type pendingReconfiguration struct{}

func (pendingReconfiguration) Error() string { return "compute reconfiguration not implemented" }

func unsigned(value any) (uint64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	result, err := strconv.ParseUint(string(number), 10, 64)
	return result, err == nil
}
func notification(raw []byte, kind string) (map[string]any, error) {
	var payload map[string]any
	if err := decodeDocument(raw, &payload); err != nil {
		return nil, err
	}
	tenant, ok := payload["tenant_id"].(string)
	if !ok || !nativeID.MatchString(tenant) {
		return nil, errors.New("invalid tenant")
	}
	if kind == "attach" {
		shards, ok := payload["shards"].([]any)
		if !ok || len(shards) == 0 || len(shards) > 256 {
			return nil, errors.New("invalid shards")
		}
		seen := map[uint64]bool{}
		for _, value := range shards {
			shard, ok := value.(map[string]any)
			if !ok {
				return nil, errors.New("invalid shard")
			}
			node, valid := unsigned(shard["node_id"])
			if !valid || node == 0 {
				return nil, errors.New("invalid node")
			}
			number, valid := unsigned(shard["shard_number"])
			if !valid || number > 255 || seen[number] {
				return nil, errors.New("invalid shard number")
			}
			seen[number] = true
		}
	} else {
		timeline, ok := payload["timeline_id"].(string)
		if !ok || !nativeID.MatchString(timeline) {
			return nil, errors.New("invalid timeline")
		}
		if _, ok := unsigned(payload["generation"]); !ok {
			return nil, errors.New("invalid generation")
		}
		keepers, ok := payload["safekeepers"].([]any)
		if !ok || len(keepers) == 0 || len(keepers) > 128 {
			return nil, errors.New("invalid safekeepers")
		}
		seen := map[uint64]bool{}
		for _, value := range keepers {
			keeper, ok := value.(map[string]any)
			if !ok {
				return nil, errors.New("invalid safekeeper")
			}
			node, valid := unsigned(keeper["id"])
			if !valid || node == 0 || seen[node] {
				return nil, errors.New("invalid safekeeper ID")
			}
			seen[node] = true
		}
	}
	return payload, nil
}

func (s *Server) notify(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r, s.config.HookToken) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64000)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		reply(w, 400, map[string]any{"error": "invalid_notification"})
		return
	}
	kind := "attach"
	if r.URL.Path == "/notify-safekeepers" {
		kind = "safekeepers"
	}
	payload, err := notification(raw, kind)
	if err != nil {
		reply(w, 400, map[string]any{"error": "invalid_notification"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if kind == "attach" {
		err = s.verifyPlacement(ctx, payload)
	}
	if err == nil {
		err = s.persist(ctx, kind, payload)
	}
	var blocked pendingReconfiguration
	if errors.As(err, &blocked) {
		reply(w, 423, map[string]any{"error": "compute_reconfiguration_pending"})
		return
	}
	if err != nil {
		s.unavailable(w, "notification", err)
		return
	}
	reply(w, 200, map[string]any{"status": "applied"})
}
func (s *Server) verifyPlacement(ctx context.Context, payload map[string]any) error {
	shards := payload["shards"].([]any)
	// Current Driver supports the one unsharded managed Pageserver, node 2.
	// Future placement changes require a real Compute reconfiguration Driver.
	if len(shards) != 1 {
		return pendingReconfiguration{}
	}
	shard := shards[0].(map[string]any)
	node, _ := unsigned(shard["node_id"])
	number, _ := unsigned(shard["shard_number"])
	if node != s.config.PageserverNodeID || number != 0 {
		return pendingReconfiguration{}
	}
	path := "/api/v1/namespaces/" + url.PathEscape(s.config.Namespace) + "/services/http:storage-controller:1234/proxy/control/v1/tenant/" + payload["tenant_id"].(string)
	state, err := s.kube.Request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	// Validate the native response, rather than acknowledging any nonempty JSON.
	authoritative, ok := state["shards"].([]any)
	if !ok || len(authoritative) != 1 {
		return errors.New("authoritative shard state unavailable")
	}
	described, ok := authoritative[0].(map[string]any)
	if !ok {
		return errors.New("invalid authoritative shard")
	}
	actual := nested(described, "node_attached")
	if fmt.Sprint(actual) != strconv.FormatUint(s.config.PageserverNodeID, 10) {
		return pendingReconfiguration{}
	}
	routes, err := s.routes(ctx)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if route.TenantID == payload["tenant_id"] && route.NodeID != 0 && route.NodeID != s.config.PageserverNodeID {
			return pendingReconfiguration{}
		}
	}
	return nil
}

func (s *Server) persist(ctx context.Context, kind string, payload map[string]any) error {
	tenant := payload["tenant_id"].(string)
	timeline, _ := payload["timeline_id"].(string)
	key := kind + ":" + tenant + ":" + timeline
	canonical, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canonical)
	next := receipt{Hash: fmt.Sprintf("%x", sum[:]), Payload: payload, ReceivedAt: float64(time.Now().UnixNano()) / 1e9, HashFormat: "go_json_v1"}
	for attempt := 0; attempt < 6; attempt++ {
		item, err := s.kube.Request(ctx, http.MethodGet, s.path("configmaps", s.config.NotificationsConfigMap), nil)
		if err != nil {
			return err
		}
		data, ok := item["data"].(map[string]any)
		if !ok {
			return errors.New("receipt data missing")
		}
		raw, ok := data["receipts.json"].(string)
		if !ok {
			return errors.New("receipt document missing")
		}
		var records map[string]receipt
		if err = decodeDocument([]byte(raw), &records); err != nil || records == nil {
			return errors.New("invalid receipt document")
		}
		if previous, ok := records[key]; ok {
			oldCanonical, err := json.Marshal(previous.Payload)
			if err != nil {
				return err
			}
			if kind == "safekeepers" {
				oldGeneration, valid := unsigned(previous.Payload["generation"])
				if !valid {
					return errors.New("previous generation invalid")
				}
				newGeneration, _ := unsigned(payload["generation"])
				if newGeneration < oldGeneration {
					return nil
				}
				if newGeneration == oldGeneration {
					// Python and Go use different whitespace in JSON hashes. Compare the
					// actual canonical payload so a language migration remains idempotent.
					if !bytes.Equal(oldCanonical, canonical) {
						return errors.New("conflicting generation")
					}
					return nil
				}
				routes, err := s.routes(ctx)
				if err != nil {
					return err
				}
				for _, route := range routes {
					if route.TenantID == tenant {
						return pendingReconfiguration{}
					}
				}
			} else if bytes.Equal(oldCanonical, canonical) {
				return nil
			}
		}
		records[key] = next
		encoded, err := json.Marshal(records)
		if err != nil {
			return err
		}
		// Kubernetes ConfigMap limit is 1 MiB. Reserve ample metadata space and fail
		// without evicting historical receipts. Database-backed receipts are future work.
		if len(encoded) > 512<<10 {
			return errors.New("receipt capacity exceeded")
		}
		data["receipts.json"] = string(encoded)
		if _, err = s.kube.Request(ctx, http.MethodPut, s.path("configmaps", s.config.NotificationsConfigMap), item); err == nil {
			return nil
		} else if !isStatus(err, http.StatusConflict) {
			return err
		}
	}
	return errors.New("receipt update conflict")
}
