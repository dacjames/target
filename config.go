package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
)

// Listen selects where a target binds. Exactly one of IP or Interface is
// normally set; if both are empty the default bind address (0.0.0.0) is used.
type Listen struct {
	IP        string `json:"ip"`
	Interface string `json:"interface"`
}

// Cert configures TLS for an HTTP target. If Cert and Key file paths are both
// set they are loaded from disk; otherwise a self-signed cert is generated for
// Hostname.
type Cert struct {
	Hostname string `json:"hostname"`
	Key      string `json:"key"`
	Cert     string `json:"cert"`
}

// TCPTarget is a TCP echo listener.
type TCPTarget struct {
	Listen  Listen `json:"listen"`
	Port    int    `json:"port"`
	UseEcho *bool  `json:"use_echo"` // nil => true
}

// UDPTarget is a UDP echo listener.
type UDPTarget struct {
	Listen  Listen `json:"listen"`
	Port    int    `json:"port"`
	UseEcho *bool  `json:"use_echo"` // nil => true
}

// HTTPTarget is an HTTP (Cert == nil) or HTTPS (Cert != nil) server.
type HTTPTarget struct {
	Listen Listen `json:"listen"`
	Port   int    `json:"port"`
	Cert   *Cert  `json:"cert"`
}

// target is one parsed, named entry from targets.json.
type target struct {
	Name string
	Type string
	TCP  *TCPTarget
	UDP  *UDPTarget
	HTTP *HTTPTarget
}

const defaultBindIP = "0.0.0.0"

// echoEnabled reports whether use_echo is on (defaults to true when unset).
func echoEnabled(p *bool) bool { return p == nil || *p }

// loadConfig reads and parses a targets.json file into a stable-ordered slice
// of targets. Malformed individual targets are reported via the returned
// error only when the whole file is unreadable/invalid JSON; per-target type
// errors are surfaced by returning them so the caller can log-and-skip.
func loadConfig(path string) ([]target, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	// {<name>: {<type>: {<params>}}}
	var outer map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &outer); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	// Stable ordering for deterministic startup/logging.
	names := make([]string, 0, len(outer))
	for name := range outer {
		names = append(names, name)
	}
	sort.Strings(names)

	targets := make([]target, 0, len(outer))
	for _, name := range names {
		inner := outer[name]
		if len(inner) != 1 {
			return nil, fmt.Errorf("target %q: expected exactly one type, got %d", name, len(inner))
		}
		for typ, params := range inner {
			t := target{Name: name, Type: typ}
			switch typ {
			case "tcp":
				var tc TCPTarget
				if err := json.Unmarshal(params, &tc); err != nil {
					return nil, fmt.Errorf("target %q tcp: %w", name, err)
				}
				t.TCP = &tc
			case "udp":
				var uc UDPTarget
				if err := json.Unmarshal(params, &uc); err != nil {
					return nil, fmt.Errorf("target %q udp: %w", name, err)
				}
				t.UDP = &uc
			case "http", "https":
				var hc HTTPTarget
				if err := json.Unmarshal(params, &hc); err != nil {
					return nil, fmt.Errorf("target %q http: %w", name, err)
				}
				t.HTTP = &hc
			default:
				return nil, fmt.Errorf("target %q: unknown type %q", name, typ)
			}
			targets = append(targets, t)
		}
	}
	return targets, nil
}

// resolveAddr turns a Listen + port into a "host:port" bind address.
// Precedence: explicit IP, then interface name (first non-loopback IPv4),
// then the default bind IP.
func resolveAddr(l Listen, port int) (string, error) {
	if port <= 0 || port > 65535 {
		return "", fmt.Errorf("invalid port %d", port)
	}
	host := defaultBindIP
	switch {
	case l.IP != "":
		host = l.IP
	case l.Interface != "":
		ip, err := interfaceIP(l.Interface)
		if err != nil {
			return "", err
		}
		host = ip
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// interfaceIP returns the first non-loopback IPv4 address on the named interface.
func interfaceIP(name string) (string, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return "", fmt.Errorf("interface %q: %w", name, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("interface %q addrs: %w", name, err)
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), nil
		}
	}
	return "", fmt.Errorf("interface %q has no non-loopback IPv4 address", name)
}
