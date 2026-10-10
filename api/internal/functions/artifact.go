package functions

import (
	"bytes"
	"errors"
	"io"
	"net/http"
)

// NewArtifactHandler serves exactly one already authorized immutable artifact.
// The surrounding control plane must resolve the instance/deployment identity
// and its dedicated key before constructing this handler. It does not infer
// authorization from a pathname or give a guest access to the S3 store.
func NewArtifactHandler(scope Scope, key []byte, digest string, archive []byte) (http.Handler, error) {
	if scope.Validate() != nil || !digestPattern.MatchString(digest) {
		return nil, errors.New("invalid immutable artifact scope")
	}
	bundle, err := ValidateBundle(archive)
	if err != nil || bundle.Digest != digest {
		return nil, errors.New("immutable artifact integrity rejected")
	}
	auth, err := NewManagerAuthenticator(key)
	if err != nil {
		return nil, err
	}
	content := bytes.Clone(archive)
	path := (Bootstrap{Scope: scope, BundleDigest: digest}).artifactPath()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(body) != 0 || r.Method != http.MethodGet || r.URL.EscapedPath() != path || r.URL.RawQuery != "" {
			managerError(w, 404, "artifact_route_rejected")
			return
		}
		if auth.Verify(r, nil) != nil {
			managerError(w, 401, "artifact_authentication_rejected")
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(content)
	}), nil
}
