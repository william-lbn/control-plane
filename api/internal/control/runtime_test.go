package control

import (
	"net/http/httptest"
	"testing"
)

func TestProcessRoleValidation(t *testing.T) {
	for _, value := range []string{"all", "api", "worker"} {
		if role, err := parseProcessRole(value); err != nil || string(role) != value {
			t.Fatalf("valid role rejected: %s", value)
		}
	}
	for _, value := range []string{"", "API", "workers", "all,api", " api", "api "} {
		if _, err := parseProcessRole(value); err == nil {
			t.Fatalf("ambiguous role accepted: %q", value)
		}
	}
}

func TestWorkerHealthHasNoPublicControlRoutes(t *testing.T) {
	s := &server{}
	handler := s.workerHealthHandler(&controllerState{})
	for _, scenario := range []struct {
		path   string
		status int
	}{
		{"/healthz", 200}, {"/readyz", 503}, {"/api/v1/auth/login", 404},
		{"/api/v1/capabilities", 404}, {"/api/docs", 404}, {"/api/v1/projects", 404},
	} {
		t.Run(scenario.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("GET", scenario.path, nil))
			if response.Code != scenario.status {
				t.Fatalf("status=%d want=%d", response.Code, scenario.status)
			}
		})
	}
}
