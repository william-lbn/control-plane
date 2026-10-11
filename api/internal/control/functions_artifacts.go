package control

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/william-lbn/control-plane/api/internal/functions"
)

var functionArtifactProject = regexp.MustCompile(`^prj_[a-f0-9]{16}$`)
var functionArtifactBranch = regexp.MustCompile(`^br_[a-f0-9]{16}$`)
var functionArtifactDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var errFunctionArtifact = errors.New("Functions immutable artifact unavailable or integrity rejected")

// The scoped backing key is internal metadata. No ZIP or storage credential is
// serialized to an Operation/audit/public deployment summary. These methods do
// not authenticate a project: their caller must authorize and retain the branch
// identity before writing the immutable backing and then admitting an intent.
type functionArtifactReference struct {
	ProjectID string
	BranchID  string
	Digest    string
	Size      int64
	Entry     string
	Key       string `json:"-"`
}

func functionArtifactKey(project, branch, digest string) (string, error) {
	if !functionArtifactProject.MatchString(project) || !functionArtifactBranch.MatchString(branch) || !functionArtifactDigest.MatchString(digest) {
		return "", errFunctionArtifact
	}
	return "functions/" + project + "/" + branch + "/sha256/" + digest + ".zip", nil
}

// reserveFunctionArtifact makes only a conditional, content-addressed PUT. The
// SDK may retry that same conditional request; it cannot overwrite an existing
// object. Success, conflict and an unknown write result all require an actual
// bounded GET/hash verification before returning a usable backing reference.
// A corrupt/foreign object is never repaired, deleted or silently adopted.
func (b *storageBlobs) reserveFunctionArtifact(ctx context.Context, project, branch string, archive []byte) (functionArtifactReference, error) {
	var empty functionArtifactReference
	if b == nil || b.client == nil || !functionArtifactProject.MatchString(project) || !functionArtifactBranch.MatchString(branch) {
		return empty, errFunctionArtifact
	}
	bundle, err := functions.ValidateBundle(archive)
	if err != nil {
		return empty, errFunctionArtifact
	}
	key, err := functionArtifactKey(project, branch, bundle.Digest)
	if err != nil {
		return empty, errFunctionArtifact
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	options := minio.PutObjectOptions{ContentType: "application/zip", DisableMultipart: true}
	options.SetMatchETagExcept("*")
	_, _ = b.client.PutObject(ctx, b.bucket, key, bytes.NewReader(archive), int64(len(archive)), options)
	// Do not return success based only on ETag, user metadata or a PUT response.
	if _, err = b.read(ctx, key, bundle.Digest, int64(len(archive))); err != nil {
		return empty, errFunctionArtifact
	}
	return functionArtifactReference{project, branch, bundle.Digest, int64(len(archive)), bundle.Entry, key}, nil
}

func (b *storageBlobs) readFunctionArtifact(ctx context.Context, ref functionArtifactReference) ([]byte, error) {
	key, err := functionArtifactKey(ref.ProjectID, ref.BranchID, ref.Digest)
	if err != nil || b == nil || b.client == nil || key != ref.Key || ref.Size < 1 || ref.Size > functions.MaxArchiveBytes || (ref.Entry != "index.mjs" && ref.Entry != "index.js") {
		return nil, errFunctionArtifact
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	archive, err := b.read(ctx, key, ref.Digest, ref.Size)
	if err != nil {
		return nil, errFunctionArtifact
	}
	bundle, err := functions.ValidateBundle(archive)
	if err != nil || bundle.Digest != ref.Digest || bundle.Entry != ref.Entry {
		return nil, errFunctionArtifact
	}
	return archive, nil
}
