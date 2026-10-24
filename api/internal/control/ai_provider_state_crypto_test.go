package control

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func providerHandleFixture() (*server, aiProviderHandle) {
	s := &server{backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32), "v2": bytes.Repeat([]byte{2}, 32)}}}
	h := aiProviderHandle{ID: "nrp_aaaaaaaaaaaaaaaaaaaaaaaa", Kind: "response", Project: "project", Org: "organization", Branch: "branch", Actor: "actor", Model: "model", Provider: "provider", Revision: "revision", ProviderIdentity: strings.Repeat("a", 64), KeyVersion: "v1", RequestID: "request", ExpiresAt: time.Unix(2000000000, 0)}
	return s, h
}

func TestAIProviderHandleEncryptionScopeAndKeyVersions(t *testing.T) {
	s, original := providerHandleFixture()
	data := aiProviderHandleData{RemoteID: "resp_owned", CreatedAt: 1900000000, Metadata: map[string]string{"topic": "owned"}}
	if err := s.aiProviderHandleSeal(&original, data); err != nil || bytes.Contains(original.Ciphertext, []byte(data.RemoteID)) {
		t.Fatal("Supplier identity was not encrypted", err)
	}
	s.backendKeys.Active = "v2"
	opened, err := s.aiProviderHandleOpen(original)
	if err != nil || opened.RemoteID != data.RemoteID || opened.Metadata["topic"] != "owned" {
		t.Fatal("Active-key update lost old reference", err)
	}
	for _, change := range []func(*aiProviderHandle){
		func(h *aiProviderHandle) { h.ID = "nrp_bbbbbbbbbbbbbbbbbbbbbbbb" },
		func(h *aiProviderHandle) { h.Project += "-other" },
		func(h *aiProviderHandle) { h.Org += "-other" },
		func(h *aiProviderHandle) { h.Branch += "-other" },
		func(h *aiProviderHandle) { h.Actor += "-other" },
		func(h *aiProviderHandle) { h.Model += "-other" },
		func(h *aiProviderHandle) { h.Provider += "-other" },
		func(h *aiProviderHandle) { h.Revision += "-other" },
		func(h *aiProviderHandle) { h.ProviderIdentity = strings.Repeat("b", 64) },
		func(h *aiProviderHandle) { h.KeyVersion = "v2" },
		func(h *aiProviderHandle) { h.RequestID += "-other" },
		func(h *aiProviderHandle) { h.ExpiresAt = h.ExpiresAt.Add(time.Second) },
		func(h *aiProviderHandle) { h.Parent = "ncv_cccccccccccccccccccccccc" },
		func(h *aiProviderHandle) { h.Previous = "nrp_dddddddddddddddddddddddd" },
	} {
		changed := original
		change(&changed)
		if _, err := s.aiProviderHandleOpen(changed); err == nil {
			t.Fatal("Ciphertext was adopted under another reference scope")
		}
	}
	tampered := original
	tampered.Ciphertext = append([]byte(nil), original.Ciphertext...)
	tampered.Ciphertext[len(tampered.Ciphertext)-1] ^= 1
	if _, err := s.aiProviderHandleOpen(tampered); err == nil {
		t.Fatal("Modified ciphertext was accepted")
	}
	delete(s.backendKeys.decoded, "v1")
	if _, err := s.aiProviderHandleOpen(original); err == nil {
		t.Fatal("Unknown historical key was silently replaced")
	}
}

func TestAIProviderHandleProofsSeparatePurposeAndIdentity(t *testing.T) {
	s, h := providerHandleFixture()
	proof, err := s.aiProviderHandleProof(h)
	if err != nil || len(proof) != 64 {
		t.Fatal(err)
	}
	remote, err := s.aiProviderRemoteMAC(h, "resp_owned")
	if err != nil || remote == proof {
		t.Fatal("Ownership and supplier-ID proofs reused a purpose", err)
	}
	other, _ := s.aiProviderRemoteMAC(h, "resp_other")
	if other == remote {
		t.Fatal("Different supplier identities shared a receipt")
	}
	h.Branch += "-other"
	changed, _ := s.aiProviderHandleProof(h)
	if changed == proof {
		t.Fatal("Supplier ownership proof was reusable across branches")
	}
}
