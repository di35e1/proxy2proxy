package main

import "testing"

func TestDecodeExternalConfigRejectsUnknownField(t *testing.T) {
	_, err := decodeExternalConfig([]byte(`{"lisetn":"127.0.0.1:1080","proxies":[]}`))
	if err == nil {
		t.Fatal("expected unknown field error")
	}
}

func TestValidateExternalConfigAcceptsHostnameUpstream(t *testing.T) {
	cfg := externalConfig{
		Listen: "0.0.0.0:1080",
		Proxies: []proxyProfile{
			{Name: "EU", Type: "socks5", Addr: "proxy.example.com:1080", User: "login", Pass: "password"},
		},
	}
	if err := validateExternalConfig(cfg); err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
}

func TestValidateExternalConfigRejectsBadValues(t *testing.T) {
	long := make([]byte, 256)
	for i := range long {
		long[i] = 'a'
	}

	cases := []externalConfig{
		{Listen: "127.0.0.1:notaport"},
		{Proxies: []proxyProfile{{Type: "socks5", Addr: "[2001:db8::1]:1080"}}},
		{Proxies: []proxyProfile{{Type: "socks5", Addr: "203.0.113.10:1080", Pass: string(long)}}},
		{Proxies: []proxyProfile{{Type: "http", Addr: "203.0.113.10:8080", User: "bad:user"}}},
		{StatusCheckTargets: []string{"example.com:notaport"}},
	}

	for i, cfg := range cases {
		if err := validateExternalConfig(cfg); err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
	}
}
