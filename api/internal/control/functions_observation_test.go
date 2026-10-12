package control

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

func TestFunctionObservationKeepsUntrustedDataAndPrivateIdentityOut(t *testing.T) {
	runtime := publishedFunctionRuntime{target: functionInstanceTarget{FunctionID: "fnc_0123456789abcdef", Generation: 9007199254740993, DeploymentID: "fdp_0123456789abcdef"}, version: 9007199254740993}
	runtime.instance.Scope.InstanceID = "fni_0123456789abcdef"
	runtime.bootstrap.MAC = "private-mac-must-not-leak"
	status := functions.InstanceStatus{NodeReady: true, LastActivity: time.Now(), NodeObservation: json.RawMessage(`{"sql_url":"private-secret-must-not-leak","node_ready":false}`)}
	data, err := json.Marshal(observedFunctionRuntime(runtime, status))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private-secret", "private-mac", "node_observation", "sql_url", "cluster_ip", "password"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatal("private/untrusted observation leaked", forbidden)
		}
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil || out["generation_token"] != "9007199254740993" || out["definition_version_token"] != "9007199254740993" || out["background_counters_trusted"] != false || out["node_ready"] != true {
		t.Fatal("trusted observation projection or exact counter token changed")
	}
}
