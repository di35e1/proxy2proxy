package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
