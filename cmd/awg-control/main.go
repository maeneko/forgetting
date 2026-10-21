// awg-control — единый бинарник Forgetting: роли ctrl / ui / agent / relay и команды оператора.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/maeneko/forgetting/internal/agent"
	"github.com/maeneko/forgetting/internal/cli"
	"github.com/maeneko/forgetting/internal/ctrl"
	"github.com/maeneko/forgetting/internal/relay"
	"github.com/maeneko/forgetting/internal/ui"
)

// version — из сборки релиза (-ldflags "-X main.version=…").
var version = "dev"

func main() {
	// Без аргументов или с командой оператора — CLI (меню, start/stop/…), как прежний awg-ctrl.
	if len(os.Args) < 2 {
		cli.Main(nil)
		return
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "ctrl":
		s, err := ctrl.New()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := s.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "ui":
		a, err := ui.New()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := a.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "agent":
		c, err := agent.ConfigFromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		a, err := agent.New(c)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		a.Run(ctx)
	case "relay":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := relay.Main(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		cli.Main(os.Args[1:])
	}
}
