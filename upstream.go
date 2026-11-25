package main

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"
)

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
