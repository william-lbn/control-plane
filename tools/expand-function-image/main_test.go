package main

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSparseExpansionPreservesBytesAndExistingFile(t *testing.T) {
	dir:=t.TempDir(); data:=append(make([]byte,1<<20),[]byte("marker")...);data=append(data,make([]byte,1<<20)...)
	var compressed bytes.Buffer;z:=gzip.NewWriter(&compressed);_,_ = z.Write(data);if err:=z.Close();err!=nil{t.Fatal(err)}
	source,target:=filepath.Join(dir,"source.gz"),filepath.Join(dir,"new.raw");if err:=os.WriteFile(source,compressed.Bytes(),0600);err!=nil{t.Fatal(err)}
	if err:=expand(source,target,int64(len(data)));err!=nil{t.Fatal(err)};actual,err:=os.ReadFile(target);if err!=nil||!bytes.Equal(actual,data){t.Fatal("sparse image changed bytes",err)}
	stat,err:=os.Stat(target);if err!=nil{t.Fatal(err)};if stat.Sys().(*syscall.Stat_t).Blocks*512>=int64(len(data))/2{t.Fatal("zero runs were materialized")}
	if expand(source,target,int64(len(data)))==nil{t.Fatal("existing image overwritten")}
}
func TestSparseExpansionRejectsCorruptionAndWrongSize(t *testing.T) {
	dir:=t.TempDir();var b bytes.Buffer;z:=gzip.NewWriter(&b);_,_=z.Write([]byte("bounded input"));_ = z.Close();source:=filepath.Join(dir,"source.gz")
	if err:=os.WriteFile(source,b.Bytes(),0600);err!=nil{t.Fatal(err)}
	if expand(source,filepath.Join(dir,"short.raw"),2)==nil{t.Fatal("oversized stream accepted")}
	if expand(source,filepath.Join(dir,"long.raw"),100)==nil{t.Fatal("short stream accepted")}
	broken:=append([]byte(nil),b.Bytes()...);broken[len(broken)-8]^=1;if err:=os.WriteFile(source,broken,0600);err!=nil{t.Fatal(err)}
	if expand(source,filepath.Join(dir,"corrupt.raw"),13)==nil{t.Fatal("bad CRC accepted")}
}
