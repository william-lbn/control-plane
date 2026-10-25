package control

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

func catalogBackendRenameDSN(raw, password, login string, p catalogRenamePayload) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "postgresql" || u.Host == "" || u.User == nil || u.User.Username() != login || u.Path != "/"+p.Name || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("original Backend database connection scope rejected")
	}
	stored, ok := u.User.Password()
	if !ok || stored != password || u.Query().Get("options") != "endpoint="+selector(p.EndpointID) || (u.Query().Get("sslmode") != "require" && u.Query().Get("sslmode") != "verify-full") {
		return "", errors.New("original Backend connection credential or Writer differs")
	}
	u.Path = "/" + p.NewName
	u.RawPath = ""
	return u.String(), nil
}

func catalogBackendRenameSecretData(old map[string]any, p catalogRenamePayload, v catalogBackendRenameDependency) (map[string]string, error) {
	data, ok := old["data"].(map[string]any)
	if !ok || len(data) == 0 || len(data) > 32 {
		return nil, errors.New("original bounded Backend private files required")
	}
	result := map[string]string{}
	for k, raw := range data {
		value, ok := raw.(string)
		if !ok {
			return nil, errors.New("invalid Backend private file")
		}
		result[k] = value
	}
	password, err := secretText(old, "password")
	if err != nil || password == "" {
		return nil, errors.New("original Backend password required")
	}
	verifier, err := secretText(old, "verifier")
	if err != nil || !functionsql.ValidVerifier(verifier) || !scramMatches(password, verifier) {
		return nil, errors.New("original Backend SCRAM identity rejected")
	}
	// A service may retain an earlier Writer. It must match its own original
	// immutable connection, not the current rename executor's endpoint.
	source := p
	source.EndpointID = v.Endpoint
	if v.Kind == "auth" {
		raw, err := secretText(old, "config.json")
		if err != nil {
			return nil, err
		}
		var config map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &config) != nil {
			return nil, errors.New("invalid original Auth private configuration")
		}
		var dsn, branch string
		if json.Unmarshal(config["databaseURL"], &dsn) != nil || json.Unmarshal(config["branchID"], &branch) != nil || branch != p.BranchID {
			return nil, errors.New("original Auth connection scope rejected")
		}
		dsn, err = catalogBackendRenameDSN(dsn, password, managedAuthLogin(p.BranchID), source)
		if err != nil {
			return nil, err
		}
		config["databaseURL"], _ = json.Marshal(dsn)
		encoded, err := json.Marshal(config)
		if err != nil {
			return nil, err
		}
		result["config.json"] = base64.StdEncoding.EncodeToString(encoded)
	} else if v.Kind == "data_api" {
		raw, err := secretText(old, "postgrest.conf")
		if err != nil {
			return nil, err
		}
		lines := strings.Split(raw, "\n")
		count := 0
		for i, line := range lines {
			if !strings.HasPrefix(strings.TrimSpace(line), "db-uri") {
				continue
			}
			if !strings.HasPrefix(line, "db-uri = ") {
				return nil, errors.New("noncanonical original Data API connection")
			}
			count++
			dsn, err := strconv.Unquote(strings.TrimPrefix(line, "db-uri = "))
			if err != nil {
				return nil, errors.New("invalid Data API private DSN")
			}
			dsn, err = catalogBackendRenameDSN(dsn, password, dataAPILogin(p.BranchID), source)
			if err != nil {
				return nil, err
			}
			lines[i] = "db-uri = " + strconv.Quote(dsn)
		}
		if count != 1 {
			return nil, errors.New("exactly one original Data API connection required")
		}
		result["postgrest.conf"] = base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\n")))
	} else {
		return nil, errors.New("unsupported Backend connection file")
	}
	return result, nil
}
