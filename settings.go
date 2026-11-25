package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

type upstreamConfig struct {
	label     string
	proxyType string
	proxyAddr string
	user      string
	pass      string
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
	cfg, err := decodeExternalConfig(data)
	if err != nil {
		return externalConfig{}, cfgPath, false, err
	}
	if err := validateExternalConfig(cfg); err != nil {
		return externalConfig{}, cfgPath, false, err
	}
	return cfg, cfgPath, false, nil
}

func decodeExternalConfig(data []byte) (externalConfig, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var cfg externalConfig
	if err := decoder.Decode(&cfg); err != nil {
		return externalConfig{}, err
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return externalConfig{}, fmt.Errorf("лишние данные после JSON-объекта")
		}
		return externalConfig{}, err
	}
	return cfg, nil
}

func parseHostPort(addr string) (string, uint16, error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "", 0, fmt.Errorf("адрес должен быть в формате host:port: %q", addr)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("некорректный порт в адресе %q", addr)
	}
	return host, uint16(port), nil
}

func validateListenAddr(addr string) error {
	if strings.TrimSpace(addr) == "" {
		return nil
	}
	host, _, err := parseHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if host == "" {
		return nil
	}
	if net.ParseIP(host) == nil {
		return fmt.Errorf("listen: host должен быть IP-адресом или пустым: %q", addr)
	}
	return nil
}

func validateProxyAddr(addr string) error {
	host, port, err := parseHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" {
		return fmt.Errorf("адрес прокси не должен быть пустым: %q", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			return fmt.Errorf("IPv6 upstream не поддерживается: %q", addr)
		}
		return validateTarget(0x01, host, port)
	}
	return validateTarget(0x03, host, port)
}

func validateStatusCheckTarget(addr string) error {
	host, port, err := parseHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" {
		return fmt.Errorf("контрольная цель не должна быть пустой: %q", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() != nil {
			return validateTarget(0x01, host, port)
		}
		return validateTarget(0x04, host, port)
	}
	return validateTarget(0x03, host, port)
}

func validateExternalConfig(cfg externalConfig) error {
	if err := validateListenAddr(cfg.Listen); err != nil {
		return err
	}

	for i, target := range cfg.StatusCheckTargets {
		if err := validateStatusCheckTarget(target); err != nil {
			return fmt.Errorf("status_check_targets[%d]: %w", i, err)
		}
	}

	for i, p := range cfg.Proxies {
		label := strings.TrimSpace(p.Name)
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
		}
		normType, err := normalizeProxyType(p.Type)
		if err != nil {
			return fmt.Errorf("proxies[%d] %s: %w", i, label, err)
		}
		if err := validateProxyAddr(p.Addr); err != nil {
			return fmt.Errorf("proxies[%d] %s: %w", i, label, err)
		}
		if normType == "socks5" && (len(p.User) > 255 || len(p.Pass) > 255) {
			return fmt.Errorf("proxies[%d] %s: логин или пароль SOCKS5 длиннее 255 байт", i, label)
		}
		if normType == "http" && strings.Contains(p.User, ":") {
			return fmt.Errorf("proxies[%d] %s: логин HTTP Basic не должен содержать ':'", i, label)
		}
	}
	return nil
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
