package control

import (
	"bytes"
	"strings"
	"testing"
)

func TestSQLHistoryNameLimitsAndEncryptionScope(t *testing.T) {
	for _, name := range []string{"", "Literal result", "查询名称", strings.Repeat("中", 80)} {
		if !validSQLHistoryName(name) {
			t.Fatal("valid name refused")
		}
	}
	for _, name := range []string{" ", " name", "name ", "bad\nname", "bad\x00name", "bad\u2028name", "bad\u2029name", strings.Repeat("x", 81), strings.Repeat("😀", 61), string([]byte{0xff})} {
		if validSQLHistoryName(name) {
			t.Fatal("invalid name admitted")
		}
	}
	s := &server{backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)}}}
	raw, err := s.sealSavedSQL("v1", "Private annotation", sqlHistoryNameAAD("project", "alice", "qh_original"))
	if err != nil {
		t.Fatal(err)
	}
	for _, aad := range [][]byte{sqlHistoryNameAAD("project", "bob", "qh_original"), sqlHistoryNameAAD("project", "alice", "qh_other"), sqlHistoryNameAAD("other", "alice", "qh_original"), sqlHistoryAAD("project", "alice", "qh_original")} {
		if _, err = s.openSavedSQL("v1", raw, aad); err == nil {
			t.Fatal("annotation scope was interchangeable")
		}
	}
	item := record{"id": "qh_original", "name_key_version": "v1", "sealed_name": raw}
	if s.openHistoryName(item, "project", "alice") != nil || item["name"] != "Private annotation" || item["sealed_name"] != nil || item["name_key_version"] != nil {
		t.Fatal("private name projection failed")
	}
}
