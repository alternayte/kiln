// Command kiln is the Kiln daemon and client.
package main

import (
	"fmt"
	"os"
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
		err = cmdCtl()
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
  serve   API server, store and VM supervisor
  ctl     CLI client for the HTTP API (lands in a later slice)
`)
}

func cmdCtl() error {
	return fmt.Errorf("ctl lands in a later slice; there is no client yet")
}
