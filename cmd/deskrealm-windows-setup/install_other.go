//go:build !windows

package main

import "errors"

func install() (string, error) {
	return "", errors.New("installation requires Windows")
}
