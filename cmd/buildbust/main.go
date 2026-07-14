// Command buildbust explains exactly which file or Dockerfile line busted
// your Docker build cache, by hashing the build context per instruction.
package main

import (
	"os"

	"github.com/JaydenCJ/buildbust/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
