//go:build live

package control

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// Explicit opt-in against the pre-provisioned restricted product account.
// Objects are retained as evidence. No bucket policy, user file, WAL, PVC or
// existing artifact is altered. This is a real backing-store gate, not proof of
// Function deployment, invocation, metadata authorization or branch cloning.
func TestFunctionArtifactLiveMinIO(t *testing.T) {
	if os.Getenv("NEON_FUNCTIONS_LIVE_ARTIFACT_TEST") != "true" {
		t.Fatal("explicit Linux live artifact admission is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	b, err := loadStorageBlobs()
	if err != nil || b == nil || b.verify(ctx) != nil {
		t.Fatal("dedicated restricted product blob configuration unavailable")
	}
	archive := functionArtifactTestZIP(t)
	project, branch := "prj_"+randomToken(8), "br_"+randomToken(8)
	ref, err := b.reserveFunctionArtifact(ctx, project, branch, archive)
	if err != nil {
		t.Fatal("real immutable MinIO write/read failed", err)
	}
	replay, err := b.reserveFunctionArtifact(ctx, project, branch, archive)
	if err != nil || replay != ref {
		t.Fatal("real conditional conflict did not recover exact content", err)
	}
	data, err := b.readFunctionArtifact(ctx, ref)
	if err != nil || !bytes.Equal(data, archive) {
		t.Fatal("retained real MinIO content not verified", err)
	}
	// A same-content replay alone cannot prove the server honors If-None-Match.
	// Attempt a conflicting conditional write to this owned disposable object,
	// require a real 412, then verify that its original bytes still remain.
	options := minio.PutObjectOptions{ContentType: "application/zip", DisableMultipart: true}
	options.SetMatchETagExcept("*")
	conflict := []byte("owned live negative probe; must not overwrite")
	_, err = b.client.PutObject(ctx, b.bucket, ref.Key, bytes.NewReader(conflict), int64(len(conflict)), options)
	if err == nil || minio.ToErrorResponse(err).StatusCode != 412 {
		t.Fatal("actual blob server did not enforce immutable conditional creation")
	}
	data, err = b.readFunctionArtifact(ctx, ref)
	if err != nil || !bytes.Equal(data, archive) {
		t.Fatal("conditional conflict changed actual retained content")
	}
	wrong := ref
	wrong.BranchID = "br_" + randomToken(8)
	if _, err = b.readFunctionArtifact(ctx, wrong); err == nil {
		t.Fatal("foreign branch backing key admitted")
	}
	wrong = ref
	wrong.Size++
	if _, err = b.readFunctionArtifact(ctx, wrong); err == nil {
		t.Fatal("wrong length accepted")
	}
	// Scope/digest/size are nonsecret receipt dimensions; no config or ZIP bytes
	// are emitted. The artifact remains attributable for future reviewed GC.
	t.Logf("retained artifact project=%s branch=%s sha256=%s bytes=%d", ref.ProjectID, ref.BranchID, ref.Digest, ref.Size)
}
