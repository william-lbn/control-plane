package control

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

type aiInputFile struct {
	ID, Project, Org, Branch, Actor, Filename, ContentType, Purpose, SHA256, KeyVersion string
	Bytes                                                                               int
	Ciphertext                                                                          []byte
}

func (s *server) aiInputCipher(version string) (cipher.AEAD, error) {
	if s.backendKeys == nil || len(s.backendKeys.decoded[version]) < 32 {
		return nil, errors.New("AI input key unavailable")
	}
	mac := hmac.New(sha256.New, s.backendKeys.decoded[version])
	mac.Write([]byte("neon-ai-input-encryption-v1"))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func aiInputAAD(file aiInputFile) []byte {
	raw, _ := json.Marshal([]any{"neon-ai-input-v1", file.ID, file.Project, file.Org, file.Branch, file.Actor, file.Filename, file.ContentType, file.Purpose, file.SHA256, file.Bytes, file.KeyVersion})
	return raw
}

func (s *server) aiInputSeal(file *aiInputFile, data []byte) error {
	sum := sha256.Sum256(data)
	file.SHA256 = hex.EncodeToString(sum[:])
	file.Bytes = len(data)
	if s.backendKeys == nil {
		return errors.New("AI input key unavailable")
	}
	file.KeyVersion = s.backendKeys.Active
	aead, err := s.aiInputCipher(file.KeyVersion)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	file.Ciphertext = aead.Seal(nonce, nonce, data, aiInputAAD(*file))
	return nil
}

func (s *server) aiInputOpen(file aiInputFile) ([]byte, error) {
	aead, err := s.aiInputCipher(file.KeyVersion)
	if err != nil || len(file.Ciphertext) < 28 {
		return nil, errors.New("AI input envelope unavailable")
	}
	data, err := aead.Open(nil, file.Ciphertext[:aead.NonceSize()], file.Ciphertext[aead.NonceSize():], aiInputAAD(file))
	if err != nil {
		return nil, errors.New("AI input envelope unavailable")
	}
	sum := sha256.Sum256(data)
	if len(data) != file.Bytes || hex.EncodeToString(sum[:]) != file.SHA256 {
		clear(data)
		return nil, errors.New("AI input digest mismatch")
	}
	return data, nil
}
