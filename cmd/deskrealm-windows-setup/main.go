package main

import (
	"errors"
	"fmt"
	"os"

	"deskrealm/internal/windowssetup"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "DeskRealm Setup:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return install()
	}
	if len(args) == 6 && args[0] == "assemble" {
		return windowssetup.Assemble(args[1], args[2], args[3], args[4], args[5])
	}
	if len(args) == 4 && args[0] == "check" {
		return windowssetup.Check(args[1], args[2], args[3])
	}
	return errors.New("invalid installer arguments")
}
