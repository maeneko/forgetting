// awg-control — единый бинарник Forgetting: роли ctrl / ui / agent и команды оператора.
package main

import (
	"fmt"
	"os"

	"github.com/maeneko/forgetting/internal/ctrl"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: awg-control <ctrl|ui|agent|start|stop|restart|status|…>")
		os.Exit(1)
	}
	switch os.Args[1] {
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
	default:
		fmt.Fprintf(os.Stderr, "awg-control: неизвестная команда %q\n", os.Args[1])
		os.Exit(1)
	}
}
