package main

import (
	"encoding/json"
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
	if len(args) == 0 || (len(args) == 1 && args[0] == "--json") {
		directory, err := install()
		if err != nil {
			return err
		}
		if len(args) == 1 {
			return json.NewEncoder(os.Stdout).Encode(map[string]string{"install_directory": directory})
		}
		fmt.Println("DeskRealm installed at", directory)
		return nil
	}
	if len(args) == 6 && args[0] == "assemble" {
		return windowssetup.Assemble(args[1], args[2], args[3], args[4], args[5])
	}
	if len(args) == 4 && args[0] == "check" {
		return windowssetup.Check(args[1], args[2], args[3])
	}
	return errors.New("invalid installer arguments")
}
