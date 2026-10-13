package control

import (
	"bytes"
	"testing"
)

func TestSavedQueryEnvelopeBindingAndRotation(t *testing.T) {
	s := &server{backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32), "v2": bytes.Repeat([]byte{2}, 32)}}}
	aad := savedQueryAAD("prj", "alice", "sq", 1)
	sql := "SELECT '世界';\nSELECT 2;"
	sealed, e := s.sealSavedSQL("v1", sql, aad)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(sealed, []byte(sql)) {
		t.Fatal("SQL stored in plaintext")
	}
	if decoded, e := s.openSavedSQL("v1", sealed, aad); e != nil || decoded != sql {
		t.Fatal("SQL not preserved exactly")
	}
	for _, bad := range [][]byte{savedQueryAAD("other", "alice", "sq", 1), savedQueryAAD("prj", "bob", "sq", 1), savedQueryAAD("prj", "alice", "other", 1), savedQueryAAD("prj", "alice", "sq", 2)} {
		if _, e = s.openSavedSQL("v1", sealed, bad); e == nil {
			t.Fatal("cross-resource ciphertext accepted")
		}
	}
	s.backendKeys.Active = "v2"
	if _, e = s.openSavedSQL("v1", sealed, aad); e != nil {
		t.Fatal("retained historical key rejected")
	}
	if _, e = s.openSavedSQL("v2", sealed, aad); e == nil {
		t.Fatal("wrong key accepted")
	}
	sealed[15] ^= 1
	if _, e = s.openSavedSQL("v1", sealed, aad); e == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}
func TestSavedQueryLimits(t *testing.T) {
	for _, q := range []savedQuerySpec{{Name: "", SQL: "x"}, {Name: "x", SQL: ""}, {Name: "x\n", SQL: "x\x00"}, {Name: "x", SQL: string([]byte{0xff})}} {
		if validSavedQuery(&q) {
			t.Fatal("invalid saved query accepted")
		}
	}
	q := savedQuerySpec{Name: "  中文草稿  ", SQL: "-- draft\nSELECT 1;"}
	if !validSavedQuery(&q) || q.Name != "中文草稿" {
		t.Fatal("valid draft rejected")
	}
}
