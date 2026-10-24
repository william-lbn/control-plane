package aigateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

var ResponseHandle = regexp.MustCompile(`^nrp_[a-f0-9]{24}$`)
var ConversationHandle = regexp.MustCompile(`^ncv_[a-f0-9]{24}$`)
var upstreamResponseID = regexp.MustCompile(`^resp_[a-zA-Z0-9_-]{1,192}$`)
var upstreamConversationID = regexp.MustCompile(`^conv_[a-zA-Z0-9_-]{1,192}$`)
var errProviderStateReceipt = errors.New("supplier response receipt unavailable")

// ProviderStateIntent contains only application-facing handles. Resolving
// their ownership is the authenticated gateway's responsibility, before it
// supplies a ProviderStateBinding. The original decoder remains stateless.
type ProviderStateIntent struct {
	Store        bool
	Previous     string
	Conversation string
	Metadata     map[string]string
}

func (p ProviderStateIntent) Requested() bool {
	return p.Store || p.Previous != "" || p.Conversation != "" || len(p.Metadata) != 0
}

// DecodeNativeProviderState strips only known top-level state parameters before
// applying the existing native protocol/media validation. Nested provider IDs
// remain forbidden; this is never a bypass for file, vector-store or item IDs.
func DecodeNativeProviderState(raw []byte, c *Config) (NativeRequest, Route, ProviderStateIntent, error) {
	var intent ProviderStateIntent
	var zero NativeRequest
	if !utf8.Valid(raw) {
		return zero, Route{}, intent, errNativeRequest
	}
	v, err := nativeObject(raw)
	if err != nil {
		return zero, Route{}, intent, err
	}
	if value, exists := v["store"]; exists {
		var ok bool
		intent.Store, ok = value.(bool)
		if !ok {
			return zero, Route{}, intent, errNativeRequest
		}
	}
	for field, destination := range map[string]*string{"previous_response_id": &intent.Previous, "conversation": &intent.Conversation} {
		value, exists := v[field]
		if !exists || value == nil {
			delete(v, field)
			continue
		}
		if field == "conversation" {
			if object, ok := value.(map[string]any); ok {
				if len(object) != 1 {
					return zero, Route{}, intent, errNativeRequest
				}
				value = object["id"]
			}
		}
		text, ok := value.(string)
		if !ok || field == "previous_response_id" && !ResponseHandle.MatchString(text) || field == "conversation" && !ConversationHandle.MatchString(text) {
			return zero, Route{}, intent, errNativeRequest
		}
		*destination = text
		delete(v, field)
	}
	if intent.Previous != "" && intent.Conversation != "" {
		return zero, Route{}, intent, errNativeRequest
	}
	if value, exists := v["metadata"]; exists {
		metadata, err := ProviderStateMetadata(value)
		if err != nil {
			return zero, Route{}, intent, err
		}
		intent.Metadata = metadata
		delete(v, "metadata")
	}
	v["store"] = false
	clean, err := json.Marshal(v)
	if err != nil {
		return zero, Route{}, intent, errNativeRequest
	}
	q, route, err := DecodeNative(clean, c, Responses, "", false)
	if err != nil || intent.Requested() && !route.AllowProviderState {
		return zero, route, intent, errNativeRequest
	}
	q.stateRequested = intent.Requested()
	q.stateIntentHash = providerStateIntentHash(intent)
	return q, route, intent, nil
}

// One of the supplier's sixteen metadata slots is reserved for the gateway's
// ownership proof. Values are not put in usage/budget logs.
func ProviderStateMetadata(value any) (map[string]string, error) {
	object, ok := value.(map[string]any)
	if !ok || len(object) > 15 {
		return nil, errNativeRequest
	}
	out := make(map[string]string, len(object))
	for key, value := range object {
		text, ok := value.(string)
		if !ok || key == "" || !utf8.ValidString(key) || !utf8.ValidString(text) || utf8.RuneCountInString(key) > 64 || utf8.RuneCountInString(text) > 512 || strings.HasPrefix(strings.ToLower(key), "_neon_") {
			return nil, errNativeRequest
		}
		out[key] = text
	}
	return out, nil
}

// StateIdentity changes when the actual supplier identity changes, including a
// static route's key. It is an internal fingerprint, never a public credential.
// Model names are omitted so a conversation may use another approved model on
// the exact same supplier account; revisions still require explicit rebinding.
func (r Route) StateIdentity() string {
	raw, _ := json.Marshal([]any{"neon-ai-provider-state-identity-v1", r.OwnerOrgID, r.ProviderID, r.RevisionID, r.ProviderType, r.BaseURL, r.key})
	sum := sha256.Sum256(raw)
	clear(raw)
	return hex.EncodeToString(sum[:])
}

// ProviderStateBinding is built only after the server verifies current branch,
// provider identity and a durable ownership receipt. Remote IDs are not client
// input. CommitResponse must durably bind an observed supplier ID before it can
// be published in JSON or in a response.created stream event.
type ProviderStateBinding struct {
	Intent             ProviderStateIntent
	ResponseID         string
	PreviousRemote     string
	ConversationRemote string
	Proof              string
	CommitResponse     func(remoteID, status string) error
	observedRemote     string
	RewriteItems       func(any) error
}

func (b ProviderStateBinding) valid() bool {
	return ResponseHandle.MatchString(b.ResponseID) && len(b.Proof) >= 32 && len(b.Proof) <= 256 && !strings.ContainsAny(b.Proof, "\r\n") &&
		(b.Intent.Previous == "" && b.PreviousRemote == "" || ResponseHandle.MatchString(b.Intent.Previous) && upstreamResponseID.MatchString(b.PreviousRemote)) &&
		(b.Intent.Conversation == "" && b.ConversationRemote == "" || ConversationHandle.MatchString(b.Intent.Conversation) && upstreamConversationID.MatchString(b.ConversationRemote)) &&
		(b.Intent.Previous == "" || b.Intent.Conversation == "") && (!b.Intent.Store || b.CommitResponse != nil)
}

func (q NativeRequest) WithProviderState(binding ProviderStateBinding) (NativeRequest, error) {
	if q.Protocol != Responses || !q.stateRequested || !binding.Intent.Requested() || !binding.valid() || q.stateIntentHash != providerStateIntentHash(binding.Intent) {
		return NativeRequest{}, errors.New("provider state binding unavailable")
	}
	body := make(map[string]any, len(q.body)+4)
	for key, value := range q.body {
		body[key] = value
	}
	body["store"] = binding.Intent.Store
	if binding.PreviousRemote != "" {
		body["previous_response_id"] = binding.PreviousRemote
	}
	if binding.ConversationRemote != "" {
		body["conversation"] = binding.ConversationRemote
	}
	metadata := make(map[string]string, len(binding.Intent.Metadata)+1)
	for key, value := range binding.Intent.Metadata {
		metadata[key] = value
	}
	metadata["_neon_binding"] = binding.Proof
	body["metadata"] = metadata
	q.body = body
	q.providerState = &binding
	return q, nil
}

func (q NativeRequest) rewriteProviderState(response map[string]any) error {
	if q.providerState == nil {
		return nil
	}
	binding := q.providerState
	id, ok := response["id"].(string)
	if !ok || !upstreamResponseID.MatchString(id) || binding.observedRemote != "" && binding.observedRemote != id {
		return errors.New("supplier response identity rejected")
	}
	metadata, ok := response["metadata"].(map[string]any)
	proof, proofOK := metadata["_neon_binding"].(string)
	if !ok || !proofOK || !hmac.Equal([]byte(proof), []byte(binding.Proof)) {
		return errors.New("supplier response ownership proof rejected")
	}
	status, ok := response["status"].(string)
	if !ok || status != "in_progress" && status != "completed" && status != "incomplete" && status != "failed" && status != "cancelled" && status != "queued" {
		return errNativeRequest
	}
	if binding.CommitResponse != nil {
		if err := binding.CommitResponse(id, status); err != nil {
			return errProviderStateReceipt
		}
	}
	binding.observedRemote = id
	delete(metadata, "_neon_binding")
	if binding.RewriteItems != nil {
		if err := binding.RewriteItems(response["output"]); err != nil {
			return errProviderStateReceipt
		}
	}
	response["id"] = binding.ResponseID
	if binding.Intent.Previous != "" {
		response["previous_response_id"] = binding.Intent.Previous
	} else {
		response["previous_response_id"] = nil
	}
	if binding.Intent.Conversation != "" {
		response["conversation"] = map[string]any{"id": binding.Intent.Conversation}
	} else {
		delete(response, "conversation")
	}
	return nil
}

func providerStateIntentHash(intent ProviderStateIntent) string {
	raw, _ := json.Marshal(intent)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Conversation inserts share the native inline media limits. They cannot
// introduce a raw remote item or a URL that bypasses branch ownership checks.
func ValidateProviderStateItems(items []any, route Route) error {
	if len(items) > 20 || !statelessInput(items, 0) || nativeTextBytes(items) > 16000 || validateNativeMedia(map[string]any{"input": items}, Responses, route) != nil {
		return errNativeRequest
	}
	for _, item := range items {
		v, ok := item.(map[string]any)
		if !ok {
			return errNativeRequest
		}
		kind, _ := v["type"].(string)
		switch kind {
		case "", "message":
			if !nativeFields(v, "type role content") || !nonemptyInput(v["content"]) {
				return errNativeRequest
			}
			role, _ := v["role"].(string)
			if role != "user" && role != "assistant" && role != "system" && role != "developer" {
				return errNativeRequest
			}
		case "function_call":
			if !nativeFields(v, "type call_id name arguments") {
				return errNativeRequest
			}
			for _, key := range []string{"call_id", "name", "arguments"} {
				if text, ok := v[key].(string); !ok || len(text) == 0 || len(text) > 16000 {
					return errNativeRequest
				}
			}
		case "function_call_output":
			if !nativeFields(v, "type call_id output") {
				return errNativeRequest
			}
			for _, key := range []string{"call_id", "output"} {
				if text, ok := v[key].(string); !ok || len(text) == 0 || len(text) > 16000 {
					return errNativeRequest
				}
			}
		default:
			return errNativeRequest
		}
	}
	return nil
}

func (q NativeRequest) rewriteProviderEvent(value map[string]any) error {
	if q.providerState == nil {
		return nil
	}
	b := q.providerState
	if b.observedRemote == "" {
		return errors.New("supplier event preceded ownership receipt")
	}
	if remote, ok := value["response_id"]; ok {
		if remote != b.observedRemote {
			return errors.New("supplier event changed response identity")
		}
		value["response_id"] = b.ResponseID
	}
	if b.RewriteItems != nil {
		for _, key := range []string{"item", "item_id"} {
			if v, ok := value[key]; ok {
				wrapper := map[string]any{key: v}
				if err := b.RewriteItems(wrapper); err != nil {
					return err
				}
				value[key] = wrapper[key]
			}
		}
	}
	return nil
}
