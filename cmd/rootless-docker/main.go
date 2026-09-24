//go:build linux

package main

import (
	"os"

	"project-alpha/internal/rootless"
)

func main() { os.Exit(rootless.Main(os.Args[1:])) }
