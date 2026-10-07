package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"project-alpha/internal/app"
	"project-alpha/internal/updater"
)

func main() {
	syscall.Umask(0077)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := app.Run(ctx, os.Args[1:]); err != nil {
		var handoff *updater.Handoff
		if errors.As(err, &handoff) {
			cancel()
			err = handoff.Exec()
		}
		log.Print(err)
		os.Exit(1)
	}
}
