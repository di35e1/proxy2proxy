package main

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

type statusTarget struct {
	host     string
	port     uint16
	addrType byte
}

func defaultStatusCheckTargets() []statusTarget {
	return []statusTarget{
		{host: "example.com", port: 443, addrType: 0x03},
		{host: "example.org", port: 443, addrType: 0x03},
		{host: "one.one.one.one", port: 443, addrType: 0x03},
	}
}

func parseStatusCheckTargets(raw []string) []statusTarget {
	targets := make([]statusTarget, 0, len(raw))
	for _, item := range raw {
		host, portText, err := net.SplitHostPort(strings.TrimSpace(item))
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port <= 0 || port > 65535 {
			continue
		}
		addrType := byte(0x03)
		if ip := net.ParseIP(host); ip != nil {
			if ip.To4() != nil {
				addrType = 0x01
			} else {
				addrType = 0x04
			}
		}
		if err := validateTarget(addrType, host, uint16(port)); err != nil {
			continue
		}
		targets = append(targets, statusTarget{host: host, port: uint16(port), addrType: addrType})
	}
	if len(targets) == 0 {
		return defaultStatusCheckTargets()
	}
	return targets
}

func setRemoteCheckingStatus(text string) {
	remoteChecking.Store(true)
	setWindowStatus(getLocalStatusLine(), text)
}

func setRemoteFinalStatus(text string, state int) {
	remoteChecking.Store(false)
	setWindowStatus(getLocalStatusLine(), text)
	setDockBadgeStatus(state)
}

func passiveSuccessFreshWithin(cfg upstreamConfig, label string, idx int, window time.Duration) bool {
	passiveSuccessMu.Lock()
	defer passiveSuccessMu.Unlock()
	return passiveSuccessCfg == cfg && passiveSuccessLabel == label && passiveSuccessIdx == idx && !passiveSuccessAt.IsZero() && time.Since(passiveSuccessAt) < window
}

func passiveSuccessFresh(cfg upstreamConfig, label string, idx int) bool {
	return passiveSuccessFreshWithin(cfg, label, idx, passiveFreshWindow)
}

func markRouteSuccess(cfg upstreamConfig, label string, idx int, passive bool) {
	wasFresh := passiveSuccessFresh(cfg, label, idx)

	if passive {
		passiveSuccessMu.Lock()
		passiveSuccessCfg = cfg
		passiveSuccessLabel = label
		passiveSuccessIdx = idx
		passiveSuccessAt = time.Now()
		passiveSuccessMu.Unlock()
	}

	if passive && routeStillCurrent(cfg, label, idx) {
		// Если окно сейчас показывает "проверка связи", свежий реальный
		// CONNECT должен сразу заменить её зелёным статусом, даже когда
		// предыдущий успех для этого маршрута ещё считался свежим.
		if !wasFresh || remoteChecking.Load() {
			setRemoteFinalStatus(fmt.Sprintf("🟢 %s (%s): доступен", cfg.label, label), dockStatusGreen)
		} else {
			setDockBadgeStatus(dockStatusGreen)
		}
	}
}

func checkRemoteOnce() bool {
	statusCheckMu.Lock()
	defer statusCheckMu.Unlock()

	idx := refreshSelectedInterface()
	cfg, label := getCurrentConfigAndIface()

	if idx == 0 {
		if routeStillCurrent(cfg, label, idx) {
			setRemoteFinalStatus(fmt.Sprintf("🔴 Интерфейс %s не активен", label), dockStatusRed)
		}
		return false
	}
	if cfg.proxyAddr == "" {
		if routeStillCurrent(cfg, label, idx) {
			setRemoteFinalStatus("🔴 Прокси не задан", dockStatusRed)
		}
		return false
	}

	// Гибридная проверка: совсем свежий успешный реальный CONNECT уже
	// доказывает, что текущий маршрут работает, поэтому синтетическую
	// проверку в этом тике не делаем. Окно подавления короткое: без
	// реального трафика активная проверка должна обновлять пинг каждые
	// statusCheckPeriod, а не ждать passiveFreshWindow.
	if passiveSuccessFreshWithin(cfg, label, idx, passiveSuppressWindow) {
		if routeStillCurrent(cfg, label, idx) {
			if remoteChecking.Load() {
				setRemoteFinalStatus(fmt.Sprintf("🟢 %s (%s): доступен", cfg.label, label), dockStatusGreen)
			} else {
				setDockBadgeStatus(dockStatusGreen)
			}
		}
		return true
	}

	// Активная часть: пробуем контрольные цели по очереди. Успех любой цели
	// означает, что интерфейс, upstream и авторизация работают; одна
	// заблокированная цель не должна красить статус в красный.
	for _, target := range statusCheckTargets {
		if passiveSuccessFresh(cfg, label, idx) {
			if routeStillCurrent(cfg, label, idx) {
				setRemoteFinalStatus(fmt.Sprintf("🟢 %s (%s): доступен", cfg.label, label), dockStatusGreen)
			}
			return true
		}
		start := time.Now()
		conn, err := dialBound(cfg.proxyAddr, idx)
		if err != nil {
			continue
		}
		if err := checkUpstreamHandshake(cfg, conn, target); err != nil {
			conn.Close()
			continue
		}
		conn.Close()
		if !routeStillCurrent(cfg, label, idx) {
			// Маршрут сменился, пока шла проверка: её успех относится к старому
			// маршруту. Возвращаем false, чтобы проверка повторилась для нового.
			return false
		}
		ms := time.Since(start).Milliseconds()
		markRouteSuccess(cfg, label, idx, false)
		setRemoteFinalStatus(fmt.Sprintf("🟢 %s (%s): доступен (%d мс)", cfg.label, label, ms), dockStatusGreen)
		return true
	}

	if passiveSuccessFresh(cfg, label, idx) {
		if routeStillCurrent(cfg, label, idx) {
			setRemoteFinalStatus(fmt.Sprintf("🟢 %s (%s): доступен", cfg.label, label), dockStatusGreen)
		}
		return true
	}
	if routeStillCurrent(cfg, label, idx) {
		setRemoteFinalStatus(fmt.Sprintf("🔴 %s (%s): нет связи", cfg.label, label), dockStatusRed)
	}
	return false
}

func checkUpstreamHandshake(cfg upstreamConfig, conn net.Conn, target statusTarget) error {
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))

	switch cfg.proxyType {
	case "http":
		_, _, err := handshakeHTTP(conn, cfg, target.host, target.port)
		return err
	case "socks5":
		_, _, err := handshakeSOCKS5(conn, cfg, target.addrType, target.host, target.port)
		return err
	default:
		return fmt.Errorf("неподдерживаемый тип прокси: %s", cfg.proxyType)
	}
}

func statusMonitorLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Паника в мониторе статуса: %v", r)
		}
	}()

	ticker := time.NewTicker(statusCheckPeriod)
	defer ticker.Stop()
	for range ticker.C {
		if !checkRemoteOnce() {
			ensureRetryLoop()
		}
	}
}

func ensureRetryLoop() {
	if !retryingRemote.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer func() {
			retryingRemote.Store(false)
			if r := recover(); r != nil {
				alertError("Ошибка проверки связи", fmt.Sprintf("%v", r), false)
			}
		}()
		for {
			time.Sleep(retryInterval)
			if checkRemoteOnce() {
				return
			}
		}
	}()
}
