package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
)

type storageGCFakeBacking struct {
	info                              minio.ObjectInfo
	inspectError, removeError         error
	present                           bool
	deletes                           int
	keepAfterDelete                   bool
	versioningError, afterDeleteError error
	onRemove                          func()
}

func (b *storageGCFakeBacking) inspect(context.Context, string) (minio.ObjectInfo, error) {
	if b.deletes > 0 && b.afterDeleteError != nil {
		return minio.ObjectInfo{}, b.afterDeleteError
	}
	if b.inspectError != nil {
		return minio.ObjectInfo{}, b.inspectError
	}
	if !b.present {
		return minio.ObjectInfo{}, minio.ErrorResponse{Code: "NoSuchKey", StatusCode: 404}
	}
	return b.info, nil
}
func (b *storageGCFakeBacking) remove(context.Context, string) error {
	b.deletes++
	if b.onRemove != nil {
		b.onRemove()
	}
	if b.removeError != nil {
		return b.removeError
	}
	if !b.keepAfterDelete {
		b.present = false
	}
	return nil
}
func (b *storageGCFakeBacking) versioningDisabled(context.Context) error { return b.versioningError }

func TestStorageGCExactOwnershipAndUnknownDelete(t *testing.T) {
	const id = "sga_012345678901234567890123"
	for _, tc := range []struct {
		name, state, reason string
		alter               func(*storageGCFakeBacking)
		deletes             int
	}{
		{"owned", "deleted", "backing_delete_verified", func(*storageGCFakeBacking) {}, 1},
		{"legacy", "held", "backing_identity_or_version_conflict", func(b *storageGCFakeBacking) { b.info.UserMetadata = nil }, 0},
		{"foreign", "held", "backing_identity_or_version_conflict", func(b *storageGCFakeBacking) { b.info.UserMetadata[storageGCMetadata] = "foreign" }, 0},
		{"ambiguous-marker", "held", "backing_identity_or_version_conflict", func(b *storageGCFakeBacking) { b.info.UserMetadata[strings.ToLower(storageGCMetadata)] = "foreign" }, 0},
		{"size-changed", "held", "backing_identity_or_version_conflict", func(b *storageGCFakeBacking) { b.info.Size++ }, 0},
		{"versioned", "held", "backing_identity_or_version_conflict", func(b *storageGCFakeBacking) { b.info.VersionID = "retained-version" }, 0},
		{"null-version", "held", "backing_identity_or_version_conflict", func(b *storageGCFakeBacking) { b.info.VersionID = "null" }, 0},
		{"missing", "absent", "backing_absent_observed", func(b *storageGCFakeBacking) { b.present = false }, 0},
		{"HEAD-denied", "delete_unknown", "backing_inspection_unknown", func(b *storageGCFakeBacking) {
			b.inspectError = minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403}
		}, 0},
		{"HEAD-transient", "delete_unknown", "backing_inspection_unknown", func(b *storageGCFakeBacking) { b.inspectError = errors.New("connection lost") }, 0},
		{"DELETE-unknown", "delete_unknown", "backing_delete_unknown", func(b *storageGCFakeBacking) { b.removeError = errors.New("connection lost") }, 1},
		{"DELETE-no-effect", "delete_unknown", "backing_still_present", func(b *storageGCFakeBacking) { b.keepAfterDelete = true }, 1},
		{"DELETE-verification-unknown", "delete_unknown", "backing_delete_verification_unknown", func(b *storageGCFakeBacking) { b.afterDeleteError = errors.New("connection lost after DELETE") }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &storageGCFakeBacking{present: true, info: minio.ObjectInfo{Size: 4, ETag: "immutable-etag", UserMetadata: map[string]string{storageGCMetadata: id}}}
			tc.alter(b)
			state, reason := storageGCCollectObject(context.Background(), b, "prj_0123456789abcdef/blob_012345678901234567890123", id, 4)
			if state != tc.state || reason != tc.reason || b.deletes != tc.deletes {
				t.Fatalf("state=%s reason=%s deletes=%d", state, reason, b.deletes)
			}
		})
	}
}

func TestStorageGCPolicyRequiresSeparateStoreAndExactApprovedScope(t *testing.T) {
	client, err := minio.New("127.0.0.1:9000", &minio.Options{Secure: false, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	ordinary := &storageBlobs{client: client, bucket: "neon-product-gc-fixture"}
	fixture := func() storageGCConfig {
		return storageGCConfig{Backing: storageBlobConfig{Endpoint: "http://127.0.0.1:9000", Bucket: ordinary.bucket, Region: "us-east-1", AccessKey: "disposable-gc-user", SecretKey: "disposable-gc-secret", LabHTTP: true}, Operators: []string{"approved_operator"}, Projects: []string{"prj_0123456789abcdef"}, MinimumAgeSeconds: 86400}
	}
	raw, _ := json.Marshal(fixture())
	if _, err = parseStorageGCDeleter(raw, ordinary); err != nil {
		t.Fatal("valid restricted policy rejected", err)
	}
	for _, tc := range []struct {
		name  string
		alter func(*storageGCConfig)
	}{
		{"no-operators", func(c *storageGCConfig) { c.Operators = nil }},
		{"duplicate-operator", func(c *storageGCConfig) { c.Operators = append(c.Operators, c.Operators[0]) }},
		{"no-project-scope", func(c *storageGCConfig) { c.Projects = nil }},
		{"wildcard-project", func(c *storageGCConfig) { c.Projects = []string{"*"} }},
		{"duplicate-project", func(c *storageGCConfig) { c.Projects = append(c.Projects, c.Projects[0]) }},
		{"no-age", func(c *storageGCConfig) { c.MinimumAgeSeconds = 0 }},
		{"age-unbounded", func(c *storageGCConfig) { c.MinimumAgeSeconds = 30*86400 + 1 }},
		{"different-store", func(c *storageGCConfig) { c.Backing.Endpoint = "http://127.0.0.1:9001" }},
		{"different-bucket", func(c *storageGCConfig) { c.Backing.Bucket = "neon-product-other" }},
		{"page-bucket", func(c *storageGCConfig) { c.Backing.Bucket = "neon-pageserver" }},
		{"implicit-http", func(c *storageGCConfig) { c.Backing.LabHTTP = false }},
		{"url-credentials", func(c *storageGCConfig) { c.Backing.Endpoint = "http://secret:secret@127.0.0.1:9000" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture()
			tc.alter(&c)
			raw, _ := json.Marshal(c)
			if _, err := parseStorageGCDeleter(raw, ordinary); err == nil {
				t.Fatal("unapproved policy accepted")
			}
		})
	}
	for _, raw := range []string{string(raw) + string(raw), strings.TrimSuffix(string(raw), "}") + `,"unknown_field":true}`} {
		if _, err := parseStorageGCDeleter([]byte(raw), ordinary); err == nil {
			t.Fatal("ambiguous policy grammar accepted")
		}
	}
}

func storageGCProofFixture() storageGCNativeProof {
	return storageGCNativeProof{ProjectID: "prj_0123456789abcdef", TenantID: strings.Repeat("1", 32), BlobKey: "prj_0123456789abcdef/blob_012345678901234567890123", NativeInventoryRevision: "native-fixture-1", FenceHeld: true, AllTimelinesEnumerated: true, Timelines: []storageGCTimelineProof{
		{TimelineID: strings.Repeat("2", 32), NoReferencesLSN: "0/FF8", MinimumReadableLSN: "0/1000", LatestLSN: "0/2000", AllDatabasesEnumerated: true, Readable: true, Directories: []storageGCDirectoryProof{{DatabaseOID: 16384, Database: "postgres", IdentityVerified: true, Complete: true}}},
		{TimelineID: strings.Repeat("3", 32), AncestorID: strings.Repeat("2", 32), NoReferencesLSN: "0/2000", MinimumReadableLSN: "0/2008", LatestLSN: "0/3000", AllDatabasesEnumerated: true, Readable: true, Directories: []storageGCDirectoryProof{{DatabaseOID: 16384, Database: "postgres", IdentityVerified: true, Complete: true}}},
	}}
}

func TestStorageGCNativeHistoryProofCannotUseCurrentBranchAbsence(t *testing.T) {
	if err := validateStorageGCNativeProof(storageGCProofFixture()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		alter func(*storageGCNativeProof)
	}{
		{"wrong-project", func(p *storageGCNativeProof) { p.ProjectID = "prj_ffffffffffffffff" }},
		{"no-native-inventory", func(p *storageGCNativeProof) { p.NativeInventoryRevision = "" }},
		{"metadata-branches-only", func(p *storageGCNativeProof) { p.AllTimelinesEnumerated = false }},
		{"no-fence", func(p *storageGCNativeProof) { p.FenceHeld = false }},
		{"unknown-publication", func(p *storageGCNativeProof) { p.UnknownPublications = true }},
		{"retained-offline-timeline", func(p *storageGCNativeProof) { p.Timelines[1].Readable = false }},
		{"retained-history", func(p *storageGCNativeProof) { p.Timelines[0].MinimumReadableLSN = "0/FF0" }},
		{"equal-horizon", func(p *storageGCNativeProof) { p.Timelines[0].MinimumReadableLSN = "0/FF8" }},
		{"invalid-lsn", func(p *storageGCNativeProof) { p.Timelines[0].MinimumReadableLSN = "not-an-lsn" }},
		{"lexical-lsn-trap", func(p *storageGCNativeProof) {
			p.Timelines[0].MinimumReadableLSN = "0/FFF"
			p.Timelines[0].NoReferencesLSN = "0/1000"
		}},
		{"future-horizon", func(p *storageGCNativeProof) { p.Timelines[0].MinimumReadableLSN = "0/3000" }},
		{"unlisted-database", func(p *storageGCNativeProof) { p.Timelines[0].AllDatabasesEnumerated = false }},
		{"unknown-outcome", func(p *storageGCNativeProof) { p.Timelines[0].Directories[0].UnknownOutcomes = true }},
		{"parent-object", func(p *storageGCNativeProof) { p.Timelines[0].Directories[0].ObjectReferences = 1 }},
		{"child-shared-object", func(p *storageGCNativeProof) { p.Timelines[1].Directories[0].ObjectReferences = 1 }},
		{"aborted-multipart", func(p *storageGCNativeProof) { p.Timelines[1].Directories[0].MultipartReferences = 1 }},
		{"other-reference", func(p *storageGCNativeProof) { p.Timelines[1].Directories[0].OtherReferences = 1 }},
		{"truncated-directory", func(p *storageGCNativeProof) { p.Timelines[1].Directories[0].Complete = false }},
		{"foreign-directory", func(p *storageGCNativeProof) { p.Timelines[1].Directories[0].IdentityVerified = false }},
		{"missing-ancestor", func(p *storageGCNativeProof) { p.Timelines[1].AncestorID = strings.Repeat("f", 32) }},
		{"cycle", func(p *storageGCNativeProof) { p.Timelines[0].AncestorID = p.Timelines[1].TimelineID }},
		{"duplicate-timeline", func(p *storageGCNativeProof) { p.Timelines[1].TimelineID = p.Timelines[0].TimelineID }},
		{"duplicate-db", func(p *storageGCNativeProof) {
			p.Timelines[0].Directories = append(p.Timelines[0].Directories, p.Timelines[0].Directories[0])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := storageGCProofFixture()
			tc.alter(&p)
			if validateStorageGCNativeProof(p) == nil {
				t.Fatal("incomplete or referenced proof admitted")
			}
		})
	}
	if _, err := (&server{}).nativeStorageGCProof(context.Background(), "prj_0123456789abcdef", "key"); !errors.Is(err, errStorageGCNativeProof) {
		t.Fatal("undeployed native proof adapter was enabled")
	}
}

func TestStorageGCNamespacesAndStoreIsolation(t *testing.T) {
	for _, key := range []string{"functions/prj_0123456789abcdef/br_one/sha256/code.zip", "prj_0123456789abcdef/legacy", "prj_0123456789abcdef/../blob_012345678901234567890123", "prj_another/blob_012345678901234567890123"} {
		if storageGCBlobKey.MatchString(key) {
			t.Fatal("unowned namespace accepted", key)
		}
	}
	if storageGCStoreID("https://one", "neon-product-fixture") == storageGCStoreID("https://two", "neon-product-fixture") || storageGCStoreID("https://one", "neon-product-one") == storageGCStoreID("https://one", "neon-product-two") {
		t.Fatal("different backing stores share GC evidence")
	}
}
