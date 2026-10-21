package control

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"regexp"
	"unicode/utf8"
)

type defaultOwnerCredentialScope struct {
	OperationID, ProjectID, BranchID, EndpointID, CredentialUID string
	Native                                                      catalogNativeScope
	DatabaseOID                                                 uint32
}

var defaultOwnerOperationID = regexp.MustCompile(`^op_[a-f0-9]{24}$`)
var defaultOwnerProjectID = regexp.MustCompile(`^prj_[a-f0-9]{16}$`)
var defaultOwnerBranchID = regexp.MustCompile(`^br_[a-f0-9]{16}$`)

func (v defaultOwnerCredentialScope) valid() bool {
	return functionResourceUID.MatchString(v.CredentialUID) && functionWriterEndpointID.MatchString(v.EndpointID) && v.Native.valid() && v.DatabaseOID > 0 && defaultOwnerOperationID.MatchString(v.OperationID) && defaultOwnerProjectID.MatchString(v.ProjectID) && defaultOwnerBranchID.MatchString(v.BranchID)
}
func (v defaultOwnerCredentialScope) aad() []byte {
	raw, _ := json.Marshal([]any{"neon-default-database-owner-credential-v1", v.OperationID, v.ProjectID, v.BranchID, v.EndpointID, v.CredentialUID, v.Native.TenantID, v.Native.TimelineID, v.DatabaseOID})
	return raw
}
func (s *server) defaultOwnerCipher(version string) (cipher.AEAD, error) {
	if s.backendKeys == nil || len(s.backendKeys.decoded[version]) < 32 {
		return nil, errors.New("default Owner credential key unavailable")
	}
	mac := hmac.New(sha256.New, s.backendKeys.decoded[version])
	mac.Write([]byte("neon-default-database-owner-credential-encryption-v1"))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func (s *server) sealDefaultOwnerPassword(scope defaultOwnerCredentialScope, password string) (string, []byte, error) {
	if !scope.valid() || password == "" || len(password) > 256 || !utf8.ValidString(password) {
		return "", nil, errors.New("invalid default Owner credential scope")
	}
	if s.backendKeys == nil {
		return "", nil, errors.New("default Owner credential key unavailable")
	}
	version := s.backendKeys.Active
	box, err := s.defaultOwnerCipher(version)
	if err != nil {
		return "", nil, err
	}
	nonce := make([]byte, box.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", nil, err
	}
	return version, box.Seal(nonce, nonce, []byte(password), scope.aad()), nil
}
func (s *server) openDefaultOwnerPassword(scope defaultOwnerCredentialScope, version string, sealed []byte) (string, error) {
	if !scope.valid() {
		return "", errors.New("invalid default Owner credential scope")
	}
	box, err := s.defaultOwnerCipher(version)
	if err != nil {
		return "", err
	}
	if len(sealed) < box.NonceSize()+box.Overhead() || len(sealed) > box.NonceSize()+box.Overhead()+256 {
		return "", errors.New("default Owner credential envelope rejected")
	}
	value, err := box.Open(nil, sealed[:box.NonceSize()], sealed[box.NonceSize():], scope.aad())
	if err != nil || len(value) == 0 || !utf8.Valid(value) {
		return "", errors.New("default Owner credential authentication rejected")
	}
	return string(value), nil
}
