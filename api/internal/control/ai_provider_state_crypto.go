package control

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"

	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

const aiProviderHandleEnvelopeLimit = 16 << 10

type aiProviderHandle struct {
	ID, Kind, Project, Org, Branch, Actor, Model, Provider, Revision string
	ProviderIdentity, KeyVersion, RequestID, Parent, Previous        string
	ExpiresAt                                                        time.Time
	Ciphertext                                                       []byte
}

type aiProviderHandleData struct {
	RemoteID  string            `json:"remote_id"`
	CreatedAt int64             `json:"created_at,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

var aiProviderRemoteID = regexp.MustCompile(`^(resp|conv)_[a-zA-Z0-9_-]{1,192}$`)
var aiProviderIdentity = regexp.MustCompile(`^[a-f0-9]{64}$`)
var errAIProviderHandleEnvelope = errors.New("provider state envelope unavailable")

func aiProviderHandleScopeValid(h aiProviderHandle) bool {
	return (h.Kind == "response" && aigateway.ResponseHandle.MatchString(h.ID) || h.Kind == "conversation" && aigateway.ConversationHandle.MatchString(h.ID)) &&
		h.Project != "" && h.Org != "" && h.Branch != "" && h.Actor != "" && h.Model != "" && h.RequestID != "" &&
		aiProviderIdentity.MatchString(h.ProviderIdentity) && h.KeyVersion != "" && !h.ExpiresAt.IsZero() &&
		(h.Parent == "" || aigateway.ConversationHandle.MatchString(h.Parent)) && (h.Previous == "" || aigateway.ResponseHandle.MatchString(h.Previous)) && (h.Parent == "" || h.Previous == "")
}

func aiProviderHandleAAD(h aiProviderHandle) []byte {
	raw, _ := json.Marshal([]any{"neon-ai-provider-handle-v1", h.ID, h.Kind, h.Project, h.Org, h.Branch, h.Actor, h.Model, h.Provider, h.Revision, h.ProviderIdentity, h.KeyVersion, h.RequestID, h.Parent, h.Previous, h.ExpiresAt.Unix()})
	return raw
}

func (s *server) aiProviderHandleKey(version, purpose string) ([]byte, error) {
	if s.backendKeys == nil || len(s.backendKeys.decoded[version]) < 32 {
		return nil, errAIProviderHandleEnvelope
	}
	mac := hmac.New(sha256.New, s.backendKeys.decoded[version])
	mac.Write([]byte("neon-ai-provider-state-" + purpose + "-v1"))
	return mac.Sum(nil), nil
}

func (s *server) aiProviderHandleCipher(version string) (cipher.AEAD, error) {
	key, err := s.aiProviderHandleKey(version, "encryption")
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errAIProviderHandleEnvelope
	}
	return cipher.NewGCM(block)
}

func aiProviderHandleDataValid(h aiProviderHandle, data aiProviderHandleData) bool {
	if !aiProviderRemoteID.MatchString(data.RemoteID) || h.Kind == "response" && data.RemoteID[:5] != "resp_" || h.Kind == "conversation" && data.RemoteID[:5] != "conv_" || data.CreatedAt < 0 || data.CreatedAt > 4102444800 {
		return false
	}
	metadata := make(map[string]any, len(data.Metadata))
	for key, value := range data.Metadata {
		metadata[key] = value
	}
	_, err := aigateway.ProviderStateMetadata(metadata)
	return err == nil
}

// The supplier ownership proof, the encryption key and the remote-ID MAC have
// independent purposes. Changing any owner/branch/provider/expiry invalidates
// the ciphertext; an active-key change does not erase older readable versions.
func (s *server) aiProviderHandleProof(h aiProviderHandle) (string, error) {
	if !aiProviderHandleScopeValid(h) {
		return "", errAIProviderHandleEnvelope
	}
	key, err := s.aiProviderHandleKey(h.KeyVersion, "ownership")
	if err != nil {
		return "", err
	}
	defer clear(key)
	mac := hmac.New(sha256.New, key)
	mac.Write(aiProviderHandleAAD(h))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func (s *server) aiProviderRemoteMAC(h aiProviderHandle, remoteID string) (string, error) {
	if !aiProviderHandleScopeValid(h) || !aiProviderRemoteID.MatchString(remoteID) {
		return "", errAIProviderHandleEnvelope
	}
	key, err := s.aiProviderHandleKey(h.KeyVersion, "remote-id")
	if err != nil {
		return "", err
	}
	defer clear(key)
	mac := hmac.New(sha256.New, key)
	mac.Write(aiProviderHandleAAD(h))
	mac.Write([]byte{0})
	mac.Write([]byte(remoteID))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func (s *server) aiProviderHandleSeal(h *aiProviderHandle, data aiProviderHandleData) error {
	if !aiProviderHandleScopeValid(*h) || !aiProviderHandleDataValid(*h, data) {
		return errAIProviderHandleEnvelope
	}
	raw, err := json.Marshal(data)
	if err != nil || len(raw) > aiProviderHandleEnvelopeLimit {
		return errAIProviderHandleEnvelope
	}
	defer clear(raw)
	aead, err := s.aiProviderHandleCipher(h.KeyVersion)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return errAIProviderHandleEnvelope
	}
	h.Ciphertext = aead.Seal(nonce, nonce, raw, aiProviderHandleAAD(*h))
	return nil
}

func (s *server) aiProviderHandleOpen(h aiProviderHandle) (aiProviderHandleData, error) {
	var data aiProviderHandleData
	if !aiProviderHandleScopeValid(h) || len(h.Ciphertext) < 28 || len(h.Ciphertext) > aiProviderHandleEnvelopeLimit+28 {
		return data, errAIProviderHandleEnvelope
	}
	aead, err := s.aiProviderHandleCipher(h.KeyVersion)
	if err != nil {
		return data, errAIProviderHandleEnvelope
	}
	raw, err := aead.Open(nil, h.Ciphertext[:aead.NonceSize()], h.Ciphertext[aead.NonceSize():], aiProviderHandleAAD(h))
	if err != nil {
		return data, errAIProviderHandleEnvelope
	}
	defer clear(raw)
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&data) != nil || d.Decode(new(any)) != io.EOF || !aiProviderHandleDataValid(h, data) {
		return aiProviderHandleData{}, errAIProviderHandleEnvelope
	}
	return data, nil
}
