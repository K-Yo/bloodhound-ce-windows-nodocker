//go:build !windows

package main

import (
	"log"
	"os"
	"os/exec"
	"runtime"
)

var consoleClose chan struct{}

func signalConsoleDone() {}

func openUI(url string) {
	if noBrowser || os.Getenv("BLOODHOUND_CE_NO_BROWSER") != "" {
		return
	}
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	if err := exec.Command(opener, url).Start(); err != nil {
		log.Printf("Could not open browser: %v", err)
	}
}

func terminate(p *os.Process) error {
	return p.Signal(os.Interrupt)
}
