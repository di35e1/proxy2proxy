package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

func setupFileLogger() {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	logDir := filepath.Join(home, "Library", "Logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	logPath := filepath.Join(logDir, "proxy2proxy.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
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
