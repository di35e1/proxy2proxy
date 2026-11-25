package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"time"
)

type unifiedLogWriter struct{}

func (unifiedLogWriter) Write(p []byte) (int, error) {
	writeUnifiedLog(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func setupLogger() {
	// macOS unified log is the system-managed place for app diagnostics:
	// Console.app and `log show` read it, and the system handles retention.
	log.SetOutput(io.MultiWriter(os.Stderr, unifiedLogWriter{}))
}

func showFallbackDialog(title, msg string) {
	esc := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		s = strings.ReplaceAll(s, "\n", `\n`)
		return s
	}
	script := fmt.Sprintf(`display dialog "%s\n\n%s" with title "%s" buttons {"OK"} default button "OK" with icon stop`,
		esc(title), esc(msg), esc(title))
	_ = exec.Command("osascript", "-e", script).Start()
}

func alertError(title, msg string, critical bool) {
	log.Printf("[%s] %s", title, msg)
	if windowRunning.Load() {
		// Окно уже работает: нативный алерт сам дождётся нажатия OK.
		// Fallback по таймауту здесь нельзя показывать, иначе через 3 секунды
		// поверх ещё открытого нативного алерта появится второй диалог.
		func() {
			defer func() {
				if recover() != nil {
					showFallbackDialog(title, msg)
				}
			}()
			showAlert(title, msg, critical)
		}()
		return
	}

	done := make(chan struct{}, 1)
	go func() {
		defer func() {
			if recover() != nil {
				showFallbackDialog(title, msg)
			}
			done <- struct{}{}
		}()
		showAlert(title, msg, critical)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		showFallbackDialog(title, msg)
	}
}

func fatalStartupError(title, msg string) {
	alertError(title, msg, true)
	os.Exit(1)
}

func recoverAndAlert(context string) {
	if r := recover(); r != nil {
		stack := string(debug.Stack())
		msg := fmt.Sprintf("%s:\n%v\n\n%s", context, r, stack)
		alertError("Ошибка proxy2proxy", msg, true)
		os.Exit(1)
	}
}
