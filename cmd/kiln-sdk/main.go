// Command kiln-sdk writes the generated clients and the OpenAPI document
// from internal/apispec. `just sdk` runs it, and a check fails when the
// files on disk differ from what it produces.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alternayte/kiln/internal/apispec"
)

func main() {
	check := flag.Bool("check", false, "fail when a file on disk differs, and write nothing")
	server := flag.String("server", "https://api.example.com", "server URL in the OpenAPI document")
	flag.Parse()

	openapi, err := apispec.OpenAPIJSON(*server)
	if err != nil {
		fail(err)
	}
	files := map[string]string{
		"sdk/openapi.json":                string(openapi) + "\n",
		"sdk/typescript/src/generated.ts": apispec.TypeScript(),
		"sdk/python/kiln/_generated.py":   apispec.Python(),
		"sdk/llms.txt":                    apispec.LLMsTXT(*server),
		"sdk/llms-full.txt":               apispec.LLMsFullTXT(*server),
	}
	for path, want := range files {
		if *check {
			got, err := os.ReadFile(path)
			if err != nil {
				fail(fmt.Errorf("%s: %w; run just sdk", path, err))
			}
			if string(got) != want {
				fail(fmt.Errorf("%s is stale; run just sdk", path))
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fail(err)
		}
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			fail(err)
		}
		fmt.Println("wrote", path)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "kiln-sdk: %v\n", err)
	os.Exit(1)
}
