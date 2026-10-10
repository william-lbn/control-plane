//go:build linux

package functions

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCustomerLogsAreBoundedUnderConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "customer.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	writer := &cappedLog{file: file, left: 1024}
	done := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			size, err := writer.Write(bytes.Repeat([]byte("x"), 4096))
			if size != 4096 || err != nil {
				t.Error("bounded log broke child pipe contract")
			}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 1024 || info.Mode().Perm() != 0600 {
		t.Fatal("customer output escaped log budget/private permissions")
	}
}
