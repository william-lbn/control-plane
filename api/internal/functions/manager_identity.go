package functions

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

func ManagerCertificateFingerprint(certificate string) string {
	block, rest := pem.Decode([]byte(certificate))
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

// ManagerIdentity contains public leaf metadata only. A certificate update
// changes neither the immutable instance nor the supervisor boot or child.
type ManagerIdentity struct {
	Scope       Scope     `json:"scope"`
	BootID      string    `json:"boot_id"`
	Fingerprint string    `json:"fingerprint"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	Renewals    int64     `json:"renewals"`
}

// The private material is carried only inside a TLS/HMAC authenticated request
// to the root supervisor. Never serialize this request into audit or errors.
type ManagerIdentityUpdate struct {
	Scope               Scope  `json:"scope"`
	BootID              string `json:"boot_id"`
	PreviousFingerprint string `json:"previous_fingerprint"`
	Certificate         string `json:"certificate"`
	PrivateKey          string `json:"private_key"`
}

type managerCertificate struct {
	mu          sync.RWMutex
	certificate *tls.Certificate
	identity    ManagerIdentity
	ca          string
}

func certificateIdentity(scope Scope, boot string, certificate tls.Certificate) (ManagerIdentity, error) {
	if scope.Validate() != nil || !bootPattern.MatchString(boot) || len(certificate.Certificate) == 0 {
		return ManagerIdentity{}, errors.New("invalid manager certificate scope")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return ManagerIdentity{}, errors.New("invalid manager certificate")
	}
	sum := sha256.Sum256(leaf.Raw)
	return ManagerIdentity{Scope: scope, BootID: boot, Fingerprint: hex.EncodeToString(sum[:]), NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter}, nil
}

// EnableIdentityRenewal is called before serving. The trust root is fixed from
// verified bootstrap. Node never receives the leaf, its key, or this handler.
func (m *Manager) EnableIdentityRenewal(b Bootstrap, now time.Time) (*tls.Config, error) {
	if b.Scope != m.scope || m.identity != nil {
		return nil, errors.New("manager identity already initialized or scope rejected")
	}
	config, err := b.ManagerTLS(now)
	if err != nil {
		return nil, err
	}
	identity, err := certificateIdentity(m.scope, m.bootID, config.Certificates[0])
	if err != nil {
		return nil, err
	}
	c := &managerCertificate{certificate: &config.Certificates[0], identity: identity, ca: b.ManagerCA}
	m.identity = c
	config.Certificates = nil
	config.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.certificate, nil
	}
	return config, nil
}

func (c *managerCertificate) current() ManagerIdentity {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.identity
}

func (m *Manager) managerIdentity(w http.ResponseWriter, r *http.Request, body []byte) {
	if m.identity == nil {
		managerError(w, 404, "manager_identity_renewal_unavailable")
		return
	}
	if r.Method == http.MethodGet && len(body) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(m.identity.current())
		return
	}
	if r.Method != http.MethodPost {
		managerError(w, 405, "manager_identity_method_rejected")
		return
	}
	var update ManagerIdentityUpdate
	if len(body) > 96<<10 || strictJSON(body, &update) != nil || update.Scope != m.scope || update.BootID != m.bootID || !digestPattern.MatchString(update.PreviousFingerprint) {
		managerError(w, 422, "manager_identity_scope_rejected")
		return
	}
	now := time.Now()
	config, err := (Bootstrap{Scope: m.scope, ManagerCA: m.identity.ca, ManagerCertificate: update.Certificate, ManagerPrivateKey: update.PrivateKey}).ManagerTLS(now)
	if err != nil {
		managerError(w, 422, "manager_identity_certificate_rejected")
		return
	}
	next, err := certificateIdentity(m.scope, m.bootID, config.Certificates[0])
	if err != nil || next.NotBefore.Before(now.Add(-2*time.Minute)) || next.NotBefore.After(now) || next.NotAfter.Before(now.Add(30*time.Minute)) || next.NotAfter.After(now.Add(2*time.Hour+time.Minute)) {
		managerError(w, 422, "manager_identity_lifetime_rejected")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.draining || m.stopping || m.stopped {
		managerError(w, 409, "instance_draining")
		return
	}
	c := m.identity
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.identity.NotAfter.After(now) {
		managerError(w, 409, "manager_identity_expired")
		return
	}
	if c.identity.Fingerprint == next.Fingerprint {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if c.identity.Fingerprint != update.PreviousFingerprint || !next.NotAfter.After(c.identity.NotAfter) || c.identity.Renewals >= 1<<31 {
		managerError(w, 409, "manager_identity_version_changed")
		return
	}
	previousLeaf, _ := x509.ParseCertificate(c.certificate.Certificate[0])
	nextLeaf, _ := x509.ParseCertificate(config.Certificates[0].Certificate[0])
	if string(previousLeaf.RawSubjectPublicKeyInfo) == string(nextLeaf.RawSubjectPublicKeyInfo) {
		managerError(w, 422, "manager_identity_fresh_key_required")
		return
	}
	next.Renewals = c.identity.Renewals + 1
	c.certificate = &config.Certificates[0]
	c.identity = next
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// ManagerIdentity observes a fresh, fully verified handshake. Metadata cannot
// claim a new leaf while the network still serves the previous certificate.
func (c *InstanceClient) ManagerIdentity(ctx context.Context) (ManagerIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c.transport.CloseIdleConnections()
	r, err := c.request(ctx, http.MethodGet, "/internal/v1/manager-identity", nil)
	if err != nil {
		return ManagerIdentity{}, err
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 8193))
	var v ManagerIdentity
	if err != nil || len(body) > 8192 || r.StatusCode != 200 || strictJSON(body, &v) != nil || v.Scope != c.scope || !bootPattern.MatchString(v.BootID) || !digestPattern.MatchString(v.Fingerprint) || v.Renewals < 0 || v.Renewals > 1<<31 || !v.NotAfter.After(v.NotBefore) || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ManagerIdentity{}, errors.New("manager identity observation rejected")
	}
	sum := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
	if hex.EncodeToString(sum[:]) != v.Fingerprint || !r.TLS.PeerCertificates[0].NotAfter.Equal(v.NotAfter) || !r.TLS.PeerCertificates[0].NotBefore.Equal(v.NotBefore) {
		return ManagerIdentity{}, errors.New("manager identity handshake changed; observe again")
	}
	return v, nil
}

// A lost reply is resolved by observing the original boot's current identity.
// This method never retries POST or weakens TLS to reach an expired peer.
func (c *InstanceClient) RenewManagerIdentity(ctx context.Context, update ManagerIdentityUpdate) error {
	if update.Scope != c.scope || !bootPattern.MatchString(update.BootID) || !digestPattern.MatchString(update.PreviousFingerprint) || len(update.Certificate) > 32<<10 || len(update.PrivateKey) > 32<<10 {
		return errors.New("manager identity update scope rejected")
	}
	body, err := json.Marshal(update)
	if err != nil {
		return errors.New("manager identity update unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r, err := c.request(ctx, http.MethodPost, "/internal/v1/manager-identity", body)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		return errors.New("manager identity update rejected; observe original boot")
	}
	return nil
}
