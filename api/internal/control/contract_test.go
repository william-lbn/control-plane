package control

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The API contract is a release boundary. Use Go's parser rather than a text
// regex, and reject either undocumented handlers or advertised missing routes.
func TestOpenAPIRouteParity(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "handlers.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (selector.Sel.Name != "Handle" && selector.Sel.Name != "HandleFunc") {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		if !ok || receiver.Name != "mux" {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			t.Error("HTTP registration must use a static method/path contract")
			return true
		}
		pattern, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Error(err)
			return true
		}
		parts := strings.SplitN(pattern, " ", 2)
		if len(parts) != 2 || parts[1] == "/api/docs" || parts[1] == "/api/openapi.json" || parts[1] == "/api/docs/assets/" {
			return true
		}
		registered[strings.ToLower(parts[0])+" "+parts[1]] = true
		return true
	})
	contract := loadOpenAPI(t)
	paths := contract["paths"].(map[string]any)
	documented := map[string]bool{}
	for path, raw := range paths {
		for method := range raw.(map[string]any) {
			switch method {
			case "get", "post", "put", "patch", "delete", "head", "options", "trace":
				documented[method+" "+path] = true
			}
		}
	}
	for route := range registered {
		if !documented[route] {
			t.Errorf("undocumented route: %s", route)
		}
	}
	for route := range documented {
		if !registered[route] {
			t.Errorf("unimplemented contract operation: %s", route)
		}
	}
}

func loadOpenAPI(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../../../contracts/openapi-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract map[string]any
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatal(err)
	}
	return contract
}

func TestOpenAPIReferencesAndSessionContract(t *testing.T) {
	contract := loadOpenAPI(t)
	var walk func(any)
	walk = func(value any) {
		switch current := value.(type) {
		case map[string]any:
			if ref, ok := current["$ref"].(string); ok {
				if !strings.HasPrefix(ref, "#/") {
					t.Errorf("contract must bundle external reference before publication: %s", ref)
				} else {
					var target any = contract
					for _, segment := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
						key := strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
						object, ok := target.(map[string]any)
						if !ok || object[key] == nil {
							t.Errorf("unresolved reference: %s", ref)
							break
						}
						target = object[key]
					}
				}
			}
			for _, child := range current {
				walk(child)
			}
		case []any:
			for _, child := range current {
				walk(child)
			}
		}
	}
	walk(contract)
	components := contract["components"].(map[string]any)
	schemes := components["securitySchemes"].(map[string]any)
	if schemes["sessionCookie"].(map[string]any)["name"] != "neon_v2_session" {
		t.Fatal("session cookie contract differs from the implemented session boundary")
	}
	// OpenAPI security names are references even though they do not use $ref.
	// A typo here would silently break generated clients and Swagger auth.
	for path, raw := range contract["paths"].(map[string]any) {
		for method, value := range raw.(map[string]any) {
			operation, ok := value.(map[string]any)
			if !ok {
				continue
			}
			security, _ := operation["security"].([]any)
			for _, entry := range security {
				for name := range entry.(map[string]any) {
					if schemes[name] == nil {
						t.Errorf("undefined security scheme %s at %s %s", name, method, path)
					}
				}
			}
		}
	}
}

func TestOpenAPIOperationIDs(t *testing.T) {
	contract := loadOpenAPI(t)
	seen := map[string]string{}
	for path, raw := range contract["paths"].(map[string]any) {
		for method, value := range raw.(map[string]any) {
			switch method {
			case "get", "post", "put", "patch", "delete", "head", "options", "trace":
				operation := value.(map[string]any)
				id, _ := operation["operationId"].(string)
				route := method + " " + path
				if id == "" {
					t.Errorf("missing operationId: %s", route)
				} else if previous := seen[id]; previous != "" {
					t.Errorf("duplicate operationId %s: %s, %s", id, previous, route)
				} else {
					seen[id] = route
				}
			}
		}
	}
}
