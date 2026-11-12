package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"project-alpha/internal/linkhost"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--help" {
		fmt.Println("alpha-interconnect-helper: receive one structured request on stdin")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := linkhost.Serve(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
