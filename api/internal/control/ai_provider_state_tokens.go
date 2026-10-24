package control

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

var aiProviderItemRemote = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,191}$`)

func (s *server) aiProviderStateCipher(version, purpose string) (cipher.AEAD, error) {
	key, err := s.aiProviderHandleKey(version, purpose)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func (s *server) sealAIProviderState(version, purpose string, aad, raw []byte) ([]byte, error) {
	aead, err := s.aiProviderStateCipher(version, purpose)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, raw, aad), nil
}
func (s *server) openAIProviderState(version, purpose string, aad, sealed []byte) ([]byte, error) {
	if len(sealed) < 28 || len(sealed) > 65564 {
		return nil, errAIProviderHandleEnvelope
	}
	aead, err := s.aiProviderStateCipher(version, purpose)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], aad)
}
func (s *server) aiProviderItemToken(h aiProviderHandle, id string) (string, error) {
	if !aiProviderItemRemote.MatchString(id) {
		return "", errors.New("invalid supplier item identity")
	}
	sealed, err := s.sealAIProviderState(h.KeyVersion, "item-handle", aiProviderHandleAAD(h), []byte(id))
	if err != nil {
		return "", err
	}
	return "nit_" + base64.RawURLEncoding.EncodeToString(sealed), nil
}
func (s *server) aiProviderItemOpen(h aiProviderHandle, token string) (string, error) {
	if !strings.HasPrefix(token, "nit_") || len(token) > 512 {
		return "", providerProblem(422, "invalid_item_reference", "Use an item reference returned for this resource")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(token[4:])
	if err != nil {
		return "", errAIProviderHandleEnvelope
	}
	raw, err := s.openAIProviderState(h.KeyVersion, "item-handle", aiProviderHandleAAD(h), sealed)
	if err != nil || !aiProviderItemRemote.Match(raw) {
		return "", providerProblem(404, "not_found", "Item does not belong to this resource")
	}
	defer clear(raw)
	return string(raw), nil
}

// Only identity fields are changed; ordinary model text remains opaque. Tokens
// bind cursors and item IDs to this exact branch, supplier and parent handle.
func (s *server) rewriteAIProviderItems(h aiProviderHandle, value any) error {
	return s.aiProviderItemsRewriter(h)(value)
}
func (s *server) aiProviderItemsRewriter(h aiProviderHandle) func(any) error {
	cache := map[string]string{}
	var walk func(any) error
	walk = func(v any) error {
		switch x := v.(type) {
		case []any:
			for _, child := range x {
				if err := walk(child); err != nil {
					return err
				}
			}
		case map[string]any:
			for key, child := range x {
				if key == "id" || key == "item_id" || key == "first_id" || key == "last_id" {
					if id, ok := child.(string); ok && id != "" {
						token := cache[id]
						if token == "" {
							var err error
							token, err = s.aiProviderItemToken(h, id)
							if err != nil {
								return err
							}
							cache[id] = token
						}
						x[key] = token
					}
				} else if key == "metadata" || key == "arguments" || key == "text" || key == "json" {
					continue
				} else if key == "response_id" {
					if h.Kind == "response" {
						x[key] = h.ID
					} else {
						delete(x, key)
					}
				} else if key == "conversation_id" {
					if h.Kind == "conversation" {
						x[key] = h.ID
					} else {
						x[key] = h.Parent
					}
				} else if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk
}

func aiProviderMutationAAD(id, actor, project, branch, handle, action, request string) []byte {
	raw, _ := json.Marshal([]string{"neon-ai-provider-state-mutation-v1", id, actor, project, branch, handle, action, request})
	return raw
}
