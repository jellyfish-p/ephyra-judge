package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/jellyfish-p/ephyra-judge/internal/attempt"
	"github.com/jellyfish-p/ephyra-judge/internal/node"
	"github.com/jellyfish-p/ephyra-judge/internal/sandbox"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	opts, err := node.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	runner := attempt.New(sandbox.DefaultHost(), opts.WorkDir)
	opts.Judge = runner.Run
	if err := node.Run(ctx, opts); err != nil {
		log.Fatal(err)
	}
}
