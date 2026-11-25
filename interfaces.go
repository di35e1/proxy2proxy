package main

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync/atomic"
)

type netPort struct {
	bsdName string
	label   string
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
