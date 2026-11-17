//go:build windows

package main

import (
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

var (
	consoleClose    = make(chan struct{}, 1)
	consoleDone     = make(chan struct{})
	consoleOnce     sync.Once
	consoleCallback uintptr
)

func init() {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	setConsoleCtrlHandler := kernel32.NewProc("SetConsoleCtrlHandler")

	consoleCallback = syscall.NewCallback(func(ctrlType uint32) uintptr {
		switch ctrlType {
		case 2, 5, 6: // CTRL_CLOSE_EVENT, CTRL_LOGOFF_EVENT, CTRL_SHUTDOWN_EVENT
			select {
			case consoleClose <- struct{}{}:
			default:
			}
			select {
			case <-consoleDone:
			case <-time.After(4 * time.Second):
			}
			return 1
		}
		return 0
	})
	setConsoleCtrlHandler.Call(consoleCallback, 1)
}

func signalConsoleDone() {
	consoleOnce.Do(func() { close(consoleDone) })
}

func openUI(url string) {
	if noBrowser {
		return
	}
	if err := exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start(); err != nil {
		log.Printf("Could not open browser: %v", err)
	}
}

func terminate(p *os.Process) error {
	return p.Kill()
}
