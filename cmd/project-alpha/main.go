package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"project-alpha/internal/app"
)

func main() {
	syscall.Umask(0077)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := app.Run(ctx, os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
