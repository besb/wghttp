package main

import (
	"bufio"
	"context"
	_ "embed"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/jessevdk/go-flags"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/zhsj/wghttp/internal/proxy"
)

//go:embed README.md
var readme string

var (
	logger *device.Logger
	opts   options
)

type options struct {
	ClientIPs  []string `long:"client-ip" env:"CLIENT_IP" env-delim:"," description:"[Interface].Address\tfor WireGuard client (can be set multiple times)"`
	ClientPort int      `long:"client-port" env:"CLIENT_PORT" description:"[Interface].ListenPort\tfor WireGuard client (optional)"`
	PrivateKey string   `long:"private-key" env:"PRIVATE_KEY" description:"[Interface].PrivateKey\tfor WireGuard client (format: base64)"`
	DNS        string   `long:"dns" env:"DNS" description:"[Interface].DNS\tfor WireGuard network (format: protocol://ip:port)\nProtocol includes udp(default), tcp, tls(DNS over TLS) and https(DNS over HTTPS)"`
	MTU        int      `long:"mtu" env:"MTU" default:"1280" description:"[Interface].MTU\tfor WireGuard network"`

	PeerEndpoint      string        `long:"peer-endpoint" env:"PEER_ENDPOINT" description:"[Peer].Endpoint\tfor WireGuard server (format: host:port)"`
	PeerKey           string        `long:"peer-key" env:"PEER_KEY" description:"[Peer].PublicKey\tfor WireGuard server (format: base64)"`
	PresharedKey      string        `long:"preshared-key" env:"PRESHARED_KEY" description:"[Peer].PresharedKey\tfor WireGuard network (optional, format: base64)"`
	KeepaliveInterval time.Duration `long:"keepalive-interval" env:"KEEPALIVE_INTERVAL" description:"[Peer].PersistentKeepalive\tfor WireGuard network (optional)"`

	ResolveDNS      string        `long:"resolve-dns" env:"RESOLVE_DNS" description:"DNS for resolving WireGuard server address (optional, format: protocol://ip:port)\nProtocol includes udp(default), tcp, tls(DNS over TLS) and https(DNS over HTTPS)"`
	ResolveInterval time.Duration `long:"resolve-interval" env:"RESOLVE_INTERVAL" default:"1m" description:"Interval for resolving WireGuard server address (set 0 to disable)"`

	Listen   string `long:"listen" env:"LISTEN" default:"localhost:8080" description:"HTTP & SOCKS5 server address"`
	ExitMode string `long:"exit-mode" env:"EXIT_MODE" choice:"remote" choice:"local" default:"remote" description:"Exit mode"`
	Verbose  bool   `short:"v" long:"verbose" description:"Show verbose debug information"`

	ClientID string `long:"client-id" env:"CLIENT_ID" hidden:"true"`
}

func main() {
	parser := flags.NewParser(&opts, flags.Default)
	parser.Usage = `[OPTIONS]

Description:`
	scanner := bufio.NewScanner(strings.NewReader(strings.TrimPrefix(readme, "# wghttp\n")))
	for scanner.Scan() {
		parser.Usage += "  " + scanner.Text() + "\n"
	}
	parser.Usage = strings.TrimSuffix(parser.Usage, "\n")
	if _, err := parser.Parse(); err != nil {
		code := 1
		if fe, ok := err.(*flags.Error); ok {
			if fe.Type == flags.ErrHelp {
				code = 0
			}
		}
		os.Exit(code)
	}
	if opts.Verbose {
		logger = device.NewLogger(device.LogLevelVerbose, "")
	} else {
		logger = device.NewLogger(device.LogLevelError, "")
	}
	logger.Verbosef("Options: %+v", opts)

	dev, tnet, err := setupNet()
	if err != nil {
		logger.Errorf("Setup netstack: %v", err)
		os.Exit(1)
	}

	listener, err := proxyListener(tnet)
	if err != nil {
		logger.Errorf("Create net listener: %v", err)
		os.Exit(1)
	}

	proxier := proxy.Proxy{
		Dial: proxyDialer(tnet), DNS: opts.DNS, Stats: stats(dev),
	}
	proxier.Serve(listener)

	os.Exit(1)
}

func proxyDialer(tnet *netstack.Net) (dialer func(ctx context.Context, network, address string) (net.Conn, error)) {
	switch opts.ExitMode {
	case "local":
		d := net.Dialer{}
		dialer = d.DialContext
	case "remote":
		dialer = tnet.DialContext
	}
	return
}

func proxyListener(tnet *netstack.Net) (net.Listener, error) {
	var tcpListener net.Listener

	tcpAddr, err := net.ResolveTCPAddr("tcp", opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("resolve listen addr: %w", err)
	}

	switch opts.ExitMode {
	case "local":
		tcpListener, err = tnet.ListenTCP(tcpAddr)
		if err != nil {
			return nil, fmt.Errorf("create listener on netstack: %w", err)
		}
	case "remote":
		tcpListener, err = net.ListenTCP("tcp", tcpAddr)
		if err != nil {
			return nil, fmt.Errorf("create listener on local net: %w", err)
		}
	}
	logger.Verbosef("Listening on %s", tcpListener.Addr())
	return tcpListener, nil
}

func setupNet() (*device.Device, *netstack.Net, error) {
	if len(opts.ClientIPs) == 0 {
		return nil, nil, fmt.Errorf("client IP is required")
	}
	var clientIPs []netip.Addr
	for _, s := range opts.ClientIPs {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return nil, nil, fmt.Errorf("parse client IP: %w", err)
		}
		clientIPs = append(clientIPs, ip)
	}

	tun, tnet, err := netstack.CreateNetTUN(clientIPs, nil, opts.MTU)
	if err != nil {
		return nil, nil, fmt.Errorf("create netstack tun: %w", err)
	}
	dev := device.NewDevice(tun, newConnBind(opts.ClientID), logger)

	if err := ipcSet(dev); err != nil {
		return nil, nil, fmt.Errorf("config device: %w", err)
	}

	if err := dev.Up(); err != nil {
		return nil, nil, fmt.Errorf("bring up device: %w", err)
	}

	return dev, tnet, nil
}
