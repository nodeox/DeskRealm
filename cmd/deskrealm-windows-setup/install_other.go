//go:build !windows

package main

import "errors"

func install() error {
	return errors.New("installation requires Windows")
}
