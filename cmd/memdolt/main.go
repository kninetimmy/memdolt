// Command memdolt is the CLI entrypoint for memdolt: durable, git-native
// per-repo project memory for coding agents, backed by Dolt.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
