package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
)

func managedAuthMailConfigured() bool {
	return os.Getenv("NEON_AUTH_MAIL_ENABLED") == "true" && regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`).MatchString(os.Getenv("NEON_AUTH_MAIL_SECRET"))
}

// The branch runtime mounts the original immutable operator Secret. SMTP
// credentials never enter metadata spec, API responses or per-branch JSON.
func (s *server) validateManagedAuthMailSecret(ctx context.Context) error {
	if !managedAuthMailConfigured() {
		return nil
	}
	value, e := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", os.Getenv("NEON_AUTH_MAIL_SECRET")), nil)
	if e != nil || value["immutable"] != true {
		return errors.New("immutable Auth mail operator Secret required")
	}
	raw, e := secretText(value, "config.json")
	if e != nil || len(raw) > 8192 || !json.Valid([]byte(raw)) {
		return errors.New("Auth mail operator configuration unavailable")
	}
	var config map[string]any
	if json.Unmarshal([]byte(raw), &config) != nil || config["version"] != float64(1) {
		return errors.New("Auth mail configuration version unsupported")
	}
	return nil // Runtime additionally enforces the complete strict SMTP schema.
}
func managedAuthMailMounted(deployment map[string]any) bool {
	if !managedAuthMailConfigured() {
		return false
	}
	volumes, _ := nested(deployment, "spec", "template", "spec", "volumes").([]any)
	volumeFound := false
	for _, v := range volumes {
		m, _ := v.(map[string]any)
		if m["name"] == "auth-mail" && nested(m, "secret", "secretName") == os.Getenv("NEON_AUTH_MAIL_SECRET") {
			volumeFound = true
		}
	}
	if !volumeFound {
		return false
	}
	containers, _ := nested(deployment, "spec", "template", "spec", "containers").([]any)
	for _, item := range containers {
		container, _ := item.(map[string]any)
		if container["name"] != "managed-auth" {
			continue
		}
		mounted, configured := false, false
		mounts, _ := container["volumeMounts"].([]any)
		for _, item := range mounts {
			mount, _ := item.(map[string]any)
			if mount["name"] == "auth-mail" && mount["mountPath"] == "/run/auth-mail" && mount["readOnly"] == true {
				mounted = true
			}
		}
		env, _ := container["env"].([]any)
		for _, item := range env {
			variable, _ := item.(map[string]any)
			if variable["name"] == "NEON_AUTH_MAIL_CONFIG_FILE" && variable["value"] == "/run/auth-mail/config.json" {
				configured = true
			}
		}
		return mounted && configured
	}
	return false
}
