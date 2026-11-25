package main

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	ipBoundIf          = 25 // IP_BOUND_IF на macOS (Darwin)
	handshakeTimeout   = 10 * time.Second
	retryInterval      = 30 * time.Second
	statusCheckPeriod  = 30 * time.Second
	passiveFreshWindow = 5 * time.Minute
	sessionDebounce    = 100 * time.Millisecond
	configFileName     = "config.json"
	maxClientConns     = 512
	acceptErrorDelay   = 100 * time.Millisecond
)

type proxyProfile struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Addr string `json:"addr"`
	User string `json:"user"`
	Pass string `json:"pass"`
}

type externalConfig struct {
	Listen             string         `json:"listen"`
	Iface              string         `json:"iface"`
	StatusCheckTargets []string       `json:"status_check_targets"`
	Proxies            []proxyProfile `json:"proxies"`
}

type statusTarget struct {
	host     string
	port     uint16
	addrType byte
}

type upstreamConfig struct {
	label     string
	proxyType string
	proxyAddr string
	user      string
	pass      string
}

type netPort struct {
	bsdName string
	label   string
}

type countingConn struct {
	net.Conn
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		atomic.AddInt64(&bytesUp, int64(n))
	}
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		atomic.AddInt64(&bytesDown, int64(n))
	}
	return n, err
}

func (c *countingConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

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

func resolveConfigPath() string {
	if exePath, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
			exePath = resolved
		}
		macOSDir := filepath.Dir(exePath)
		contentsDir := filepath.Dir(macOSDir)
		appDir := filepath.Dir(contentsDir)
		if strings.HasSuffix(appDir, ".app") && filepath.Base(macOSDir) == "MacOS" {
			return filepath.Join(filepath.Dir(appDir), configFileName)
		}
		return filepath.Join(macOSDir, configFileName)
	}
	return configFileName
}

func loadExternalConfig() (externalConfig, string, bool, error) {
	cfgPath := resolveConfigPath()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return externalConfig{}, cfgPath, true, nil
		}
		return externalConfig{}, cfgPath, false, err
	}
	var cfg externalConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return externalConfig{}, cfgPath, false, err
	}
	return cfg, cfgPath, false, nil
}

func normalizeProxyType(raw string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(raw))
	if t == "socks" {
		t = "socks5"
	}
	if t != "http" && t != "socks5" {
		return "", fmt.Errorf("неизвестный тип прокси: %s", raw)
	}
	return t, nil
}

func formatRouteInfo(listenAddr string, cfg upstreamConfig, ifaceDisplay string) string {
	return fmt.Sprintf("Маршрут: %s → %s (%s://%s) через %s. Закройте окно, чтобы остановить сервис.",
		listenAddr, cfg.label, cfg.proxyType, cfg.proxyAddr, ifaceDisplay)
}

func isInterfaceActive(bsdName string) bool {
	iface, err := net.InterfaceByName(bsdName)
	if err != nil || (iface.Flags&net.FlagUp) == 0 {
		return false
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && !ipNet.IP.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

func listHardwarePorts(defaultBsd string) []netPort {
	out, err := exec.Command("networksetup", "-listallhardwareports").Output()
	if err != nil {
		return nil
	}

	var ports []netPort
	var currentPort string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Hardware Port:") {
			currentPort = strings.TrimSpace(strings.TrimPrefix(line, "Hardware Port:"))
		} else if strings.HasPrefix(line, "Device:") {
			dev := strings.TrimSpace(strings.TrimPrefix(line, "Device:"))
			if dev != "" && (dev == defaultBsd || isInterfaceActive(dev)) {
				label := dev
				if currentPort != "" {
					label = fmt.Sprintf("%s %s", dev, currentPort)
				}
				ports = append(ports, netPort{bsdName: dev, label: label})
			}
			currentPort = ""
		}
	}
	return ports
}

func findPortLabel(ports []netPort, bsdName string) string {
	for _, p := range ports {
		if p.bsdName == bsdName {
			return p.label
		}
	}
	return bsdName
}

func getCurrentConfigAndIface() (upstreamConfig, string) {
	stateMu.RLock()
	cfg := globalCfg
	ifaceLabel := currentIfaceLabel
	stateMu.RUnlock()
	return cfg, ifaceLabel
}

// routeStillCurrent сообщает, что маршрут, для которого шла проверка,
// всё ещё выбран: пока проверка ходила в сеть, конфиг, подпись интерфейса
// и его индекс не сменились. Иначе результат проверки устарел и статус
// по нему ставить нельзя.
func routeStillCurrent(cfg upstreamConfig, label string, idx int) bool {
	currentCfg, currentLabel := getCurrentConfigAndIface()
	return currentCfg == cfg && currentLabel == label && int(atomic.LoadInt64(&currentIfaceIndex)) == idx
}

// refreshSelectedInterface заново находит выбранный интерфейс по имени и
// обновляет его индекс. Это позволяет запуску и retry-циклу восстановиться,
// если интерфейс из конфига отсутствовал при старте, а потом появился или
// пользователь выбрал другой интерфейс.
func refreshSelectedInterface() int {
	stateMu.RLock()
	name := currentIfaceName
	stateMu.RUnlock()

	if name == "" {
		atomic.StoreInt64(&currentIfaceIndex, 0)
		return 0
	}

	iface, err := net.InterfaceByName(name)
	if err != nil || !interfaceReady(iface) {
		atomic.StoreInt64(&currentIfaceIndex, 0)
		return 0
	}

	atomic.StoreInt64(&currentIfaceIndex, int64(iface.Index))
	return iface.Index
}

func interfaceReady(iface *net.Interface) bool {
	if (iface.Flags & net.FlagUp) == 0 {
		return false
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && !ipNet.IP.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

func registerSession(conn net.Conn) {
	sessionMu.Lock()
	activeSessions[conn] = sessionGeneration
	sessionMu.Unlock()
}

func unregisterSession(conn net.Conn) {
	sessionMu.Lock()
	delete(activeSessions, conn)
	sessionMu.Unlock()
}

func closeActiveSessions() {
	sessionMu.Lock()
	sessionGeneration++
	currentGeneration := sessionGeneration
	var oldSessions []net.Conn
	for conn, generation := range activeSessions {
		if generation < currentGeneration {
			oldSessions = append(oldSessions, conn)
		}
	}
	sessionMu.Unlock()

	for _, conn := range oldSessions {
		conn.Close()
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

func passiveSuccessFresh(cfg upstreamConfig, label string, idx int) bool {
	passiveSuccessMu.Lock()
	defer passiveSuccessMu.Unlock()
	return passiveSuccessCfg == cfg && passiveSuccessLabel == label && passiveSuccessIdx == idx && !passiveSuccessAt.IsZero() && time.Since(passiveSuccessAt) < passiveFreshWindow
}

func markRouteSuccess(cfg upstreamConfig, label string, idx int, passive bool) {
	wasFresh := passiveSuccessFresh(cfg, label, idx)

	passiveSuccessMu.Lock()
	passiveSuccessCfg = cfg
	passiveSuccessLabel = label
	passiveSuccessIdx = idx
	passiveSuccessAt = time.Now()
	passiveSuccessMu.Unlock()

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

	// Гибридная проверка: свежий успешный реальный CONNECT уже доказывает,
	// что текущий маршрут работает, поэтому синтетическую проверку не делаем.
	if passiveSuccessFresh(cfg, label, idx) {
		if routeStillCurrent(cfg, label, idx) {
			setRemoteFinalStatus(fmt.Sprintf("🟢 %s (%s): доступен", cfg.label, label), dockStatusGreen)
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

func dialBound(addr string, ifaceIndex int) (net.Conn, error) {
	if ifaceIndex <= 0 {
		return nil, fmt.Errorf("некорректный индекс интерфейса")
	}
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			var sockErr error
			err := c.Control(func(fd uintptr) {
				sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipBoundIf, ifaceIndex)
			})
			if err != nil {
				return err
			}
			return sockErr
		},
	}
	return dialer.Dial("tcp", addr)
}

func handleClient(clientConn net.Conn) {
	atomic.AddInt64(&activeConns, 1)
	registerSession(clientConn)
	notifySessionCountChanged()
	defer func() {
		unregisterSession(clientConn)
		atomic.AddInt64(&activeConns, -1)
		notifySessionCountChanged()
		clientConn.Close()
	}()

	_ = clientConn.SetDeadline(time.Now().Add(handshakeTimeout))

	buf := make([]byte, 258)
	if _, err := io.ReadFull(clientConn, buf[:2]); err != nil {
		return
	}
	if buf[0] != 0x05 {
		return
	}
	nMethods := int(buf[1])
	if _, err := io.ReadFull(clientConn, buf[:nMethods]); err != nil {
		return
	}
	noAuthOffered := false
	for _, method := range buf[:nMethods] {
		if method == 0x00 {
			noAuthOffered = true
			break
		}
	}
	if !noAuthOffered {
		_, _ = clientConn.Write([]byte{0x05, 0xff})
		return
	}
	if _, err := clientConn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	if _, err := io.ReadFull(clientConn, buf[:4]); err != nil {
		return
	}
	if buf[0] != 0x05 || buf[2] != 0x00 {
		return
	}
	if buf[1] != 0x01 {
		_, _ = clientConn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	addrType := buf[3]
	var targetHost string
	switch addrType {
	case 0x01:
		if _, err := io.ReadFull(clientConn, buf[:4]); err != nil {
			return
		}
		targetHost = net.IP(buf[:4]).String()
	case 0x03:
		if _, err := io.ReadFull(clientConn, buf[:1]); err != nil {
			return
		}
		domainLen := int(buf[0])
		if _, err := io.ReadFull(clientConn, buf[:domainLen]); err != nil {
			return
		}
		targetHost = string(buf[:domainLen])
	case 0x04:
		if _, err := io.ReadFull(clientConn, buf[:16]); err != nil {
			return
		}
		targetHost = net.IP(buf[:16]).String()
	default:
		_, _ = clientConn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	if _, err := io.ReadFull(clientConn, buf[:2]); err != nil {
		return
	}
	targetPort := binary.BigEndian.Uint16(buf[:2])
	if err := validateTarget(addrType, targetHost, targetPort); err != nil {
		_, _ = clientConn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	cfg, ifaceLabel := getCurrentConfigAndIface()
	ifaceIdx := refreshSelectedInterface()
	upstreamConn, preBuffered, err := connectUpstream(cfg, ifaceIdx, addrType, targetHost, targetPort)
	if err != nil {
		// Ошибка одного реального соединения может быть проблемой конкретного
		// сайта, поэтому не красим статус сразу: активная проверка решит,
		// жив ли маршрут.
		ensureRetryLoop()
		_, _ = clientConn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstreamConn.Close()
	markRouteSuccess(cfg, ifaceLabel, ifaceIdx, true)

	_ = clientConn.SetDeadline(time.Time{})
	_ = upstreamConn.SetDeadline(time.Time{})

	if _, err := clientConn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	if len(preBuffered) > 0 {
		if _, err := clientConn.Write(preBuffered); err != nil {
			return
		}
	}

	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(upstreamConn, clientConn)
		if err == nil {
			closeWrite(upstreamConn)
		}
		errCh <- err
	}()
	go func() {
		_, err := io.Copy(clientConn, upstreamConn)
		if err == nil {
			closeWrite(clientConn)
		}
		errCh <- err
	}()
	if err := <-errCh; err == nil {
		<-errCh
	}
}

func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func validateTarget(addrType byte, host string, port uint16) error {
	if port == 0 {
		return fmt.Errorf("некорректный порт назначения")
	}

	switch addrType {
	case 0x01:
		ip := net.ParseIP(host)
		if ip == nil || ip.To4() == nil {
			return fmt.Errorf("некорректный IPv4-адрес назначения")
		}
	case 0x03:
		if host == "" || len(host) > 255 {
			return fmt.Errorf("некорректное доменное имя назначения")
		}
		for i := 0; i < len(host); i++ {
			c := host[i]
			if c < 0x21 || c > 0x7e || strings.ContainsRune(`:/?#@[]\`, rune(c)) {
				return fmt.Errorf("некорректное доменное имя назначения")
			}
		}
	case 0x04:
		ip := net.ParseIP(host)
		if ip == nil || ip.To16() == nil {
			return fmt.Errorf("некорректный IPv6-адрес назначения")
		}
	default:
		return fmt.Errorf("неподдерживаемый тип адреса назначения")
	}

	return nil
}

func connectUpstream(cfg upstreamConfig, ifaceIdx int, addrType byte, targetHost string, targetPort uint16) (net.Conn, []byte, error) {
	rawConn, err := dialBound(cfg.proxyAddr, ifaceIdx)
	if err != nil {
		_, label := getCurrentConfigAndIface()
		if routeStillCurrent(cfg, label, ifaceIdx) {
			setRemoteFinalStatus(fmt.Sprintf("🔴 %s (%s): нет связи", cfg.label, label), dockStatusRed)
		}
		ensureRetryLoop()
		return nil, nil, err
	}

	_ = rawConn.SetDeadline(time.Now().Add(handshakeTimeout))

	switch cfg.proxyType {
	case "http":
		return handshakeHTTP(rawConn, cfg, targetHost, targetPort)

	case "socks5":
		return handshakeSOCKS5(rawConn, cfg, addrType, targetHost, targetPort)

	default:
		rawConn.Close()
		return nil, nil, fmt.Errorf("неподдерживаемый тип прокси: %s", cfg.proxyType)
	}
}

func handshakeHTTP(conn net.Conn, cfg upstreamConfig, targetHost string, targetPort uint16) (net.Conn, []byte, error) {
	targetAddr := net.JoinHostPort(targetHost, strconv.Itoa(int(targetPort)))
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", targetAddr, targetAddr)
	if cfg.user != "" || cfg.pass != "" {
		auth := base64.StdEncoding.EncodeToString([]byte(cfg.user + ":" + cfg.pass))
		req += fmt.Sprintf("Proxy-Authorization: Basic %s\r\n", auth)
	}
	req += "\r\n"

	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, nil, err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		conn.Close()
		return nil, nil, fmt.Errorf("HTTP статус %d", resp.StatusCode)
	}

	var leftover []byte
	if n := br.Buffered(); n > 0 {
		leftover, _ = br.Peek(n)
	}
	return conn, leftover, nil
}

func handshakeSOCKS5(conn net.Conn, cfg upstreamConfig, addrType byte, targetHost string, targetPort uint16) (net.Conn, []byte, error) {
	hasAuth := cfg.user != "" || cfg.pass != ""
	if hasAuth {
		if _, err := conn.Write([]byte{0x05, 0x02, 0x00, 0x02}); err != nil {
			conn.Close()
			return nil, nil, err
		}
	} else {
		if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
			conn.Close()
			return nil, nil, err
		}
	}

	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if resp[0] != 0x05 {
		conn.Close()
		return nil, nil, fmt.Errorf("неверная версия SOCKS ответа: %d", resp[0])
	}

	switch resp[1] {
	case 0x00:
	case 0x02:
		if !hasAuth {
			conn.Close()
			return nil, nil, fmt.Errorf("удалённый SOCKS5 требует логин и пароль")
		}
		if len(cfg.user) > 255 || len(cfg.pass) > 255 {
			conn.Close()
			return nil, nil, fmt.Errorf("логин или пароль длиннее 255 байт")
		}
		authReq := make([]byte, 0, 3+len(cfg.user)+len(cfg.pass))
		authReq = append(authReq, 0x01, byte(len(cfg.user)))
		authReq = append(authReq, cfg.user...)
		authReq = append(authReq, byte(len(cfg.pass)))
		authReq = append(authReq, cfg.pass...)

		if _, err := conn.Write(authReq); err != nil {
			conn.Close()
			return nil, nil, err
		}
		if _, err := io.ReadFull(conn, resp); err != nil {
			conn.Close()
			return nil, nil, err
		}
		if resp[0] != 0x01 || resp[1] != 0x00 {
			conn.Close()
			return nil, nil, fmt.Errorf("ошибка авторизации на SOCKS5 (код %d)", resp[1])
		}
	default:
		conn.Close()
		return nil, nil, fmt.Errorf("удалённый SOCKS5 отклонил методы авторизации (код %d)", resp[1])
	}

	req := []byte{0x05, 0x01, 0x00, addrType}
	switch addrType {
	case 0x01:
		ip := net.ParseIP(targetHost).To4()
		if ip == nil {
			conn.Close()
			return nil, nil, fmt.Errorf("некорректный IPv4-адрес назначения")
		}
		req = append(req, ip...)
	case 0x03:
		if len(targetHost) > 255 {
			conn.Close()
			return nil, nil, fmt.Errorf("доменное имя слишком длинное")
		}
		req = append(req, byte(len(targetHost)))
		req = append(req, targetHost...)
	case 0x04:
		ip := net.ParseIP(targetHost).To16()
		if ip == nil {
			conn.Close()
			return nil, nil, fmt.Errorf("некорректный IPv6-адрес назначения")
		}
		req = append(req, ip...)
	}

	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, targetPort)
	req = append(req, portBytes...)

	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, nil, err
	}

	replyHeader := make([]byte, 4)
	if _, err := io.ReadFull(conn, replyHeader); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if replyHeader[0] != 0x05 || replyHeader[2] != 0x00 {
		conn.Close()
		return nil, nil, fmt.Errorf("некорректный ответ SOCKS5")
	}
	if replyHeader[1] != 0x00 {
		conn.Close()
		return nil, nil, fmt.Errorf("удалённый SOCKS5 отклонил CONNECT (код %d)", replyHeader[1])
	}

	var skipLen int
	switch replyHeader[3] {
	case 0x01:
		skipLen = 4 + 2
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			conn.Close()
			return nil, nil, err
		}
		skipLen = int(l[0]) + 2
	case 0x04:
		skipLen = 16 + 2
	default:
		conn.Close()
		return nil, nil, fmt.Errorf("неизвестный тип адреса в ответе SOCKS5: %d", replyHeader[3])
	}

	if _, err := io.ReadFull(conn, make([]byte, skipLen)); err != nil {
		conn.Close()
		return nil, nil, err
	}

	return conn, nil, nil
}
