package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

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
