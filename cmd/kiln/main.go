// Command kiln is the Kiln daemon and client.
package main

import (
	"fmt"
	"os"

	"github.com/alternayte/kiln/internal/apispec"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "serve":
		err = cmdServe()
	case "snapfault":
		err = cmdSnapfault(os.Args[2:])
	case "ctl":
		err = cmdCtl(os.Args[2:])
	case "ca":
		err = cmdCA(os.Args[2:])
	case "gateway-env":
		err = cmdGatewayEnv(os.Args[2:])
	case "version", "-v", "--version":
		// The one constant that changes with every release, so a binary and
		// the document the gateway serves can never disagree.
		fmt.Println(apispec.Version)
		return
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "kiln: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "kiln: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: kiln <command> [args]

commands:
  init    first-run setup: fetch the pinned Firecracker and kernel, write config.json
  serve   API server, store, supervisor and reconciler
  ctl     CLI client for the HTTP API
  ca      issue and revoke the client certificate one gateway uses
  gateway-env  print the address, token and certificates one gateway needs
  version print the release this binary came from
`)
}
