// Command tailraid is the single binary for the tailraid distributed filesystem: node daemon,
// NFS/S3 gateway and management CLI. At milestone M0 it only reports its version; the cobra
// command tree lands with M1.
package main

import (
	"fmt"
	"os"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("tailraid", version)
		return
	}
	fmt.Fprintln(os.Stderr, "tailraid: milestone M0 (design docs); commands land with M1. Try: tailraid version")
	os.Exit(2)
}
