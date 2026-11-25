package main

import (
	"fmt"
	"log"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ipBoundIf             = 25 // IP_BOUND_IF на macOS (Darwin)
	handshakeTimeout      = 10 * time.Second
	retryInterval         = 30 * time.Second
	statusCheckPeriod     = 30 * time.Second
	passiveSuppressWindow = 30 * time.Second
	passiveFreshWindow    = 5 * time.Minute
	sessionDebounce       = 100 * time.Millisecond
	configFileName        = "config.json"
	maxClientConns        = 512
	acceptErrorDelay      = 100 * time.Millisecond
)

var (
	activeConns          int64
	bytesDown            int64
	bytesUp              int64
	downRateBps          int64
	upRateBps            int64
	currentIfaceIndex    int64
	retryingRemote       atomic.Bool
	windowRunning        atomic.Bool
	sessionNotifyTimerMu sync.Mutex
	sessionNotifyTimer   *time.Timer
	stateMu              sync.RWMutex
	localState           string
	currentIfaceName     string
	currentIfaceLabel    string
	globalCfg            upstreamConfig
	globalListenAddr     string
	proxyProfiles        []upstreamConfig
	statusCheckTargets   = defaultStatusCheckTargets()
	statusCheckMu        sync.Mutex
	passiveSuccessMu     sync.Mutex
	passiveSuccessCfg    upstreamConfig
	passiveSuccessLabel  string
	passiveSuccessIdx    int
	passiveSuccessAt     time.Time
	remoteChecking       atomic.Bool
	clientSlots          = make(chan struct{}, maxClientConns)
	sessionMu            sync.Mutex
	sessionGeneration    int64
	activeSessions       = make(map[net.Conn]int64)
)

func main() {
	runtime.LockOSThread()
	setupFileLogger()
	defer recoverAndAlert("Сбой в главном потоке")

	extCfg, cfgPath, cfgMissing, cfgErr := loadExternalConfig()
	if cfgErr != nil {
		fatalStartupError("Ошибка конфига", fmt.Sprintf("Не удалось прочитать %s:\n%v", cfgPath, cfgErr))
	}

	listenAddr := "127.0.0.1:1080"
	if extCfg.Listen != "" {
		listenAddr = extCfg.Listen
	}
	ifaceName := "en0"
	if extCfg.Iface != "" {
		ifaceName = extCfg.Iface
	}
	statusCheckTargets = parseStatusCheckTargets(extCfg.StatusCheckTargets)

	profiles := make([]upstreamConfig, 0, len(extCfg.Proxies))
	for i, p := range extCfg.Proxies {
		normType, err := normalizeProxyType(p.Type)
		if err != nil || strings.TrimSpace(p.Addr) == "" {
			continue
		}
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = fmt.Sprintf("#%d %s://%s", i+1, normType, strings.TrimSpace(p.Addr))
		}
		profiles = append(profiles, upstreamConfig{
			label:     name,
			proxyType: normType,
			proxyAddr: strings.TrimSpace(p.Addr),
			user:      p.User,
			pass:      p.Pass,
		})
	}

	if len(profiles) == 0 {
		if cfgMissing {
			fatalStartupError(
				"Не найден config.json",
				fmt.Sprintf("Положите config.json рядом с приложением:\n%s", cfgPath),
			)
		}
		fatalStartupError(
			"Нет валидных прокси в config.json",
			fmt.Sprintf("В файле %s не найдено ни одного профиля с корректными полями \"type\" (http/socks5) и \"addr\".", cfgPath),
		)
	}

	activeIdx := 0

	proxyProfiles = profiles
	globalListenAddr = listenAddr

	stateMu.Lock()
	globalCfg = proxyProfiles[activeIdx]
	currentIfaceName = ifaceName
	currentIfaceLabel = ifaceName
	stateMu.Unlock()

	ports := listHardwarePorts(ifaceName)
	ifaceDisplay := findPortLabel(ports, ifaceName)
	stateMu.Lock()
	currentIfaceLabel = ifaceDisplay
	stateMu.Unlock()

	infoText := formatRouteInfo(listenAddr, proxyProfiles[activeIdx], ifaceDisplay)
	setupWindow("Proxy2Proxy", infoText)

	foundDefault := false
	for _, p := range ports {
		isDef := p.bsdName == ifaceName
		if isDef {
			foundDefault = true
		}
		addWindowInterfaceOption(p.label, p.bsdName, isDef)
	}
	if !foundDefault && ifaceName != "" {
		addWindowInterfaceOption(ifaceName, ifaceName, true)
	}

	for i, p := range proxyProfiles {
		addWindowProxyOption(p.label, i == activeIdx)
	}

	startSpeedTicker()

	go func() {
		defer recoverAndAlert("Сбой в сетевом потоке прокси")
		startProxyServer()
	}()

	runWindow()
}

func startSpeedTicker() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Паника в счётчике скорости: %v", r)
			}
		}()
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		var prevDown, prevUp int64
		for range ticker.C {
			d := atomic.SwapInt64(&bytesDown, 0)
			u := atomic.SwapInt64(&bytesUp, 0)
			atomic.StoreInt64(&downRateBps, d)
			atomic.StoreInt64(&upRateBps, u)

			if d != prevDown || u != prevUp {
				prevDown = d
				prevUp = u
				setLocalWindowStatus(getLocalStatusLine())
			}
		}
	}()
}

func formatSpeed(bps int64) string {
	const (
		kb = 1024
		mb = 1024 * 1024
		gb = 1024 * 1024 * 1024
	)
	switch {
	case bps >= gb:
		return fmt.Sprintf("%.2f ГБ/с", float64(bps)/gb)
	case bps >= mb:
		return fmt.Sprintf("%.1f МБ/с", float64(bps)/mb)
	default:
		return fmt.Sprintf("%d КБ/с", bps/kb)
	}
}

func switchInterface(bsdName string) {
	defer recoverAndAlert("Ошибка переключения интерфейса")

	ports := listHardwarePorts(bsdName)
	label := findPortLabel(ports, bsdName)

	stateMu.Lock()
	currentIfaceName = bsdName
	currentIfaceLabel = label
	cfg := globalCfg
	stateMu.Unlock()

	setWindowInfo(formatRouteInfo(globalListenAddr, cfg, label))

	iface, err := net.InterfaceByName(bsdName)
	if err != nil {
		atomic.StoreInt64(&currentIfaceIndex, 0)
		closeActiveSessions()
		setRemoteFinalStatus(fmt.Sprintf("🔴 Интерфейс %s не найден в системе", label), dockStatusRed)
		ensureRetryLoop()
		return
	}

	atomic.StoreInt64(&currentIfaceIndex, int64(iface.Index))
	closeActiveSessions()
	setRemoteCheckingStatus(fmt.Sprintf("⏳ Проверка связи через %s...", label))
	if !checkRemoteOnce() {
		ensureRetryLoop()
	}
}

func switchProxy(index int) {
	defer recoverAndAlert("Ошибка переключения прокси")

	if index < 0 || index >= len(proxyProfiles) {
		return
	}

	stateMu.Lock()
	globalCfg = proxyProfiles[index]
	cfg := globalCfg
	ifaceLabel := currentIfaceLabel
	stateMu.Unlock()

	setWindowInfo(formatRouteInfo(globalListenAddr, cfg, ifaceLabel))
	closeActiveSessions()
	setRemoteCheckingStatus(fmt.Sprintf("⏳ Проверка %s (%s)...", cfg.label, ifaceLabel))
	if !checkRemoteOnce() {
		ensureRetryLoop()
	}
}

func startProxyServer() {
	setLocalState("⏳ Локальный SOCKS5: запуск...")
	setWindowStatus(getLocalStatusLine(), "Удалённый прокси: ожидание сети...")

	// Listener запускается независимо от готовности выбранного интерфейса:
	// если интерфейс из конфига отсутствует, локальный SOCKS всё равно должен
	// слушать порт, а пользователь сможет переключиться на существующий
	// интерфейс в окне. Доступность маршрута покажет checkRemoteOnce.
	listener, err := net.Listen("tcp", globalListenAddr)
	if err != nil {
		errMsg := fmt.Sprintf("Не удалось открыть порт %s:\n%v", globalListenAddr, err)
		setLocalState("❌ Локальный SOCKS5: порт занят или недоступен")
		remoteChecking.Store(false)
		setWindowStatus(getLocalStatusLine(), err.Error())
		setDockBadgeStatus(dockStatusRed)
		alertError("Ошибка запуска сервера", errMsg, true)
		return
	}
	defer listener.Close()

	setLocalState("🟢 Локальный SOCKS5")
	setRemoteCheckingStatus("⏳ Удалённый прокси: проверка связи...")

	if !checkRemoteOnce() {
		ensureRetryLoop()
	}
	go statusMonitorLoop()

	for {
		clientConn, err := listener.Accept()
		if err != nil {
			log.Printf("Ошибка accept: %v", err)
			time.Sleep(acceptErrorDelay)
			continue
		}

		select {
		case clientSlots <- struct{}{}:
		default:
			clientConn.Close()
			continue
		}

		if tcpConn, ok := clientConn.(*net.TCPConn); ok {
			_ = tcpConn.SetKeepAlive(true)
			_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
		}

		go func(c net.Conn) {
			defer func() {
				<-clientSlots
				if r := recover(); r != nil {
					log.Printf("Паника в сессии клиента: %v", r)
				}
			}()
			handleClient(&countingConn{Conn: c})
		}(clientConn)
	}
}

func setLocalState(s string) {
	stateMu.Lock()
	localState = s
	stateMu.Unlock()
	setLocalWindowStatus(getLocalStatusLine())
}

func getLocalStatusLine() string {
	stateMu.RLock()
	base := localState
	stateMu.RUnlock()
	active := atomic.LoadInt64(&activeConns)
	down := atomic.LoadInt64(&downRateBps)
	up := atomic.LoadInt64(&upRateBps)
	return fmt.Sprintf("%s. Сессий: %d | Скорость: ↓ %s ↑ %s", base, active, formatSpeed(down), formatSpeed(up))
}

func notifySessionCountChanged() {
	sessionNotifyTimerMu.Lock()
	defer sessionNotifyTimerMu.Unlock()

	if sessionNotifyTimer != nil {
		sessionNotifyTimer.Stop()
	}
	sessionNotifyTimer = time.AfterFunc(sessionDebounce, func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Паника в таймере сессий: %v", r)
			}
		}()
		setLocalWindowStatus(getLocalStatusLine())
	})
}
