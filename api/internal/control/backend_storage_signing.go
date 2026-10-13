package control

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
)

func backendStorageScope(scopes []string) bool {
	for _, scope := range scopes {
		if scope == "storage:read" || scope == "storage:write" {
			return true
		}
	}
	return false
}
func storageSigningSecret(token string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("neon-storage-sigv4-secret-v1"))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func storageSigningAAD(id string, generation int64) []byte {
	value, _ := json.Marshal([]any{"backend-storage-token-v1", id, generation})
	return value
}
func (s *server) storageSigningCipher(version string) (cipher.AEAD, error) {
	if s.backendKeys == nil || s.backendKeys.decoded[version] == nil {
		return nil, errors.New("storage signing key unavailable")
	}
	mac := hmac.New(sha256.New, s.backendKeys.decoded[version])
	mac.Write([]byte("neon-storage-token-encryption-v1"))
	block, e := aes.NewCipher(mac.Sum(nil))
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(block)
}
func (s *server) storeStorageSigning(ctx context.Context, tx pgx.Tx, id string, generation int64, token string) error {
	c, e := s.storageSigningCipher(s.backendKeys.Active)
	if e != nil {
		return e
	}
	nonce := make([]byte, c.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return e
	}
	sealed := c.Seal(nonce, nonce, []byte(token), storageSigningAAD(id, generation))
	_, e = tx.Exec(ctx, `INSERT INTO backend_storage_signing(credential_id,generation,key_version,sealed_token) VALUES($1,$2,$3,$4) ON CONFLICT(credential_id) DO UPDATE SET generation=$2,key_version=$3,sealed_token=$4,updated_at=now()`, id, generation, s.backendKeys.Active, sealed)
	return e
}
func (s *server) loadStorageSigning(ctx context.Context, id string) (token, anchor string, err error) {
	var version string
	var sealed []byte
	var generation int64
	err = s.db.QueryRow(ctx, `SELECT z.key_version,z.sealed_token,z.generation,c.branch_id FROM backend_storage_signing z JOIN backend_credentials c ON c.id=z.credential_id AND c.generation=z.generation WHERE c.id=$1 AND c.revoked_at IS NULL AND c.expires_at>clock_timestamp()`, id).Scan(&version, &sealed, &generation, &anchor)
	if err != nil {
		return "", "", err
	}
	c, e := s.storageSigningCipher(version)
	if e != nil || len(sealed) < c.NonceSize()+c.Overhead() {
		return "", "", errors.New("storage signing envelope unavailable")
	}
	value, e := c.Open(nil, sealed[:c.NonceSize()], sealed[c.NonceSize():], storageSigningAAD(id, generation))
	if e != nil || !backendTokenPattern.Match(value) {
		return "", "", errBackendCredentialInvalid
	}
	return string(value), anchor, nil
}
