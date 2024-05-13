package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/tinkerbell/ipxedust"
	"github.com/tinkerbell/ipxedust/ihttp"
	"github.com/tinkerbell/smee/internal/dhcp/handler"
	"github.com/tinkerbell/smee/internal/dhcp/handler/proxy"
	"github.com/tinkerbell/smee/internal/dhcp/handler/reservation"
	"github.com/tinkerbell/smee/internal/dhcp/server"
	"github.com/tinkerbell/smee/internal/ipxe/http"
	"github.com/tinkerbell/smee/internal/ipxe/script"
	"github.com/tinkerbell/smee/internal/metric"
	"github.com/tinkerbell/smee/internal/otel"
	"github.com/tinkerbell/smee/internal/syslog"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
)

var (
	// GitRev is the git revision of the build. It is set by the Makefile.
	GitRev = "unknown (use make)"

	startTime = time.Now()
)

const name = "smee"

type config struct {
	syslog         syslogConfig
	tftp           tftp
	ipxeHTTPBinary ipxeHTTPBinary
	ipxeHTTPScript ipxeHTTPScript
	dhcp           dhcpConfig

	// loglevel is the log level for smee.
	logLevel string
	backends dhcpBackends
	otel     otelConfig
}

type syslogConfig struct {
	enabled  bool
	bindAddr string
}

type tftp struct {
	bindAddr        string
	blockSize       int
	enabled         bool
	ipxeScriptPatch string
	timeout         time.Duration
}

type ipxeHTTPBinary struct {
	enabled bool
}

type ipxeHTTPScript struct {
	enabled                       bool
	bindAddr                      string
	extraKernelArgs               string
	hookURL                       string
	tinkServer                    string
	tinkServerUseTLS              bool
	trustedProxies                string
	disableDiscoverTrustedProxies bool
	retries                       int
	retryDelay                    int
}

type dhcpConfig struct {
	enabled           bool
	mode              string
	bindAddr          string
	bindInterface     string
	ipForPacket       string
	syslogIP          string
	tftpIP            string
	httpIpxeBinaryURL string
	httpIpxeScript    httpIpxeScript
}

type httpIpxeScript struct {
	url string
	// injectMacAddress will prepend the hardware mac address to the ipxe script URL file name.
	// For example: http://1.2.3.4/my/loc/auto.ipxe -> http://1.2.3.4/my/loc/40:15:ff:89:cc:0e/auto.ipxe
	// Setting this to false is useful when you are not using the auto.ipxe script in Smee.
	injectMacAddress bool
}

type dhcpBackends struct {
	file       File
	kubernetes Kube
}

type otelConfig struct {
	endpoint string
	insecure bool
}

func main() {
	cfg := &config{}
	cli := newCLI(cfg, flag.NewFlagSet(name, flag.ExitOnError))
	_ = cli.Parse(os.Args[1:])

	log := defaultLogger(cfg.logLevel)
	log.Info("starting", "version", GitRev)

	ctx, done := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	defer done()
	oCfg := otel.Config{
		Servicename: "smee",
		Endpoint:    cfg.otel.endpoint,
		Insecure:    cfg.otel.insecure,
		Logger:      log,
	}
	ctx, otelShutdown, err := otel.Init(ctx, oCfg)
	if err != nil {
		log.Error(err, "failed to initialize OpenTelemetry")
		panic(err)
	}
	defer otelShutdown()
	metric.Init()

	g, ctx := errgroup.WithContext(ctx)
	// syslog
	if cfg.syslog.enabled {
		log.Info("starting syslog server", "bind_addr", cfg.syslog.bindAddr)
		g.Go(func() error {
			if err := syslog.StartReceiver(ctx, log, cfg.syslog.bindAddr, 1); err != nil {
				log.Error(err, "syslog server failure")
				return err
			}
			<-ctx.Done()
			log.Info("syslog server stopped")
			return nil
		})
	}

	// tftp
	if cfg.tftp.enabled {
		tftpServer := &ipxedust.Server{
			Log:                  log.WithValues("service", "github.com/tinkerbell/smee").WithName("github.com/tinkerbell/ipxedust"),
			HTTP:                 ipxedust.ServerSpec{Disabled: true}, // disabled because below we use the http handlerfunc instead.
			EnableTFTPSinglePort: true,
		}
		tftpServer.EnableTFTPSinglePort = true
		if ip, err := netip.ParseAddrPort(cfg.tftp.bindAddr); err == nil {
			tftpServer.TFTP = ipxedust.ServerSpec{
				Disabled:  false,
				Addr:      ip,
				Timeout:   cfg.tftp.timeout,
				Patch:     []byte(cfg.tftp.ipxeScriptPatch),
				BlockSize: cfg.tftp.blockSize,
			}
			// start the ipxe binary tftp server
			log.Info("starting tftp server", "bind_addr", cfg.tftp.bindAddr)
			g.Go(func() error {
				return tftpServer.ListenAndServe(ctx)
			})
		} else {
			log.Error(err, "invalid bind address")
			panic(fmt.Errorf("invalid bind address: %w", err))
		}
	}

	handlers := http.HandlerMapping{}
	// http ipxe binaries
	if cfg.ipxeHTTPBinary.enabled {
		// serve ipxe binaries from the "/ipxe/" URI.
		handlers["/ipxe/"] = ihttp.Handler{
			Log:   log.WithValues("service", "github.com/tinkerbell/smee").WithName("github.com/tinkerbell/ipxedust"),
			Patch: []byte(cfg.tftp.ipxeScriptPatch),
		}.Handle
	}

	// http ipxe script
	if cfg.ipxeHTTPScript.enabled {
		var br handler.BackendReader
		switch {
		case cfg.backends.file.Enabled && cfg.backends.kubernetes.Enabled:
			panic("only one backend can be enabled at a time")
		case cfg.backends.file.Enabled:
			b, err := cfg.backends.file.backend(ctx, log)
			if err != nil {
				panic(fmt.Errorf("failed to run file backend: %w", err))
			}
			br = b
		default: // default backend is kubernetes
			b, err := cfg.backends.kubernetes.backend(ctx)
			if err != nil {
				panic(fmt.Errorf("failed to run kubernetes backend: %w", err))
			}
			br = b
		}

		jh := script.Handler{
			Logger:               log,
			Backend:              br,
			OSIEURL:              cfg.ipxeHTTPScript.hookURL,
			ExtraKernelParams:    strings.Split(cfg.ipxeHTTPScript.extraKernelArgs, " "),
			PublicSyslogFQDN:     cfg.dhcp.syslogIP,
			TinkServerTLS:        cfg.ipxeHTTPScript.tinkServerUseTLS,
			TinkServerGRPCAddr:   cfg.ipxeHTTPScript.tinkServer,
			IPXEScriptRetries:    cfg.ipxeHTTPScript.retries,
			IPXEScriptRetryDelay: cfg.ipxeHTTPScript.retryDelay,
		}
		// serve ipxe script from the "/" URI.
		handlers["/"] = jh.HandlerFunc()
	}

	if len(handlers) > 0 {
		// start the http server for ipxe binaries and scripts
		tp := parseTrustedProxies(cfg.ipxeHTTPScript.trustedProxies)
		httpServer := &http.Config{
			GitRev:         GitRev,
			StartTime:      startTime,
			Logger:         log,
			TrustedProxies: tp,
		}
		log.Info("serving http", "addr", cfg.ipxeHTTPScript.bindAddr, "trusted_proxies", tp)
		g.Go(func() error {
			return httpServer.ServeHTTP(ctx, cfg.ipxeHTTPScript.bindAddr, handlers)
		})
	}

	// dhcp serving
	if cfg.dhcp.enabled {
		dh, err := cfg.dhcpHandler(ctx, log)
		if err != nil {
			log.Error(err, "failed to create dhcp listener")
			panic(fmt.Errorf("failed to create dhcp listener: %w", err))
		}
		log.Info("starting dhcp server", "bind_addr", cfg.dhcp.bindAddr)
		g.Go(func() error {
			bindAddr, err := netip.ParseAddrPort(cfg.dhcp.bindAddr)
			if err != nil {
				panic(fmt.Errorf("invalid tftp address for DHCP server: %w", err))
			}
			conn, err := server4.NewIPv4UDPConn(cfg.dhcp.bindInterface, net.UDPAddrFromAddrPort(bindAddr))
			if err != nil {
				panic(err)
			}
			defer conn.Close()
			ds := &server.DHCP{Logger: log, Conn: conn, Handlers: []server.Handler{dh}}

			return ds.Serve(ctx)
		})
	}

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		log.Error(err, "failed running all Smee services")
		panic(err)
	}
	log.Info("smee is shutting down")
}

func (c *config) dhcpHandler(ctx context.Context, log logr.Logger) (server.Handler, error) {
	// 1. create the handler
	// 2. create the backend
	// 3. add the backend to the handler
	pktIP, err := netip.ParseAddr(c.dhcp.ipForPacket)
	if err != nil {
		return nil, fmt.Errorf("invalid bind address: %w", err)
	}
	tftpIP, err := netip.ParseAddrPort(c.dhcp.tftpIP)
	if err != nil {
		return nil, fmt.Errorf("invalid tftp address for DHCP server: %w", err)
	}
	httpBinaryURL, err := url.Parse(c.dhcp.httpIpxeBinaryURL)
	if err != nil || httpBinaryURL == nil {
		return nil, fmt.Errorf("invalid http ipxe binary url: %w", err)
	}
	httpScriptURL, err := url.Parse(c.dhcp.httpIpxeScript.url)
	if err != nil || httpScriptURL == nil {
		return nil, fmt.Errorf("invalid http ipxe script url: %w", err)
	}
	ipxeScript := func(d *dhcpv4.DHCPv4) *url.URL {
		return httpScriptURL
	}
	if c.dhcp.httpIpxeScript.injectMacAddress {
		ipxeScript = func(d *dhcpv4.DHCPv4) *url.URL {
			u := *httpScriptURL
			p := path.Base(u.Path)
			u.Path = path.Join(path.Dir(u.Path), d.ClientHWAddr.String(), p)
			return &u
		}
	}
	var backend handler.BackendReader
	switch {
	case c.backends.file.Enabled && c.backends.kubernetes.Enabled:
		panic("only one backend can be enabled at a time")
	case c.backends.file.Enabled:
		b, err := c.backends.file.backend(ctx, log)
		if err != nil {
			return nil, fmt.Errorf("failed to create file backend: %w", err)
		}
		backend = b
	default: // default backend is kubernetes
		b, err := c.backends.kubernetes.backend(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to create kubernetes backend: %w", err)
		}
		backend = b
	}
	switch c.dhcp.mode {
	case "reservation":
		syslogIP, err := netip.ParseAddr(c.dhcp.syslogIP)
		if err != nil {
			return nil, fmt.Errorf("invalid syslog address: %w", err)
		}
		dh := &reservation.Handler{
			Backend: backend,
			IPAddr:  pktIP,
			Log:     log,
			Netboot: reservation.Netboot{
				IPXEBinServerTFTP: tftpIP,
				IPXEBinServerHTTP: httpBinaryURL,
				IPXEScriptURL:     ipxeScript,
				Enabled:           true,
			},
			OTELEnabled: true,
			SyslogAddr:  syslogIP,
		}
		return dh, nil
	case "proxy":
		dh := &proxy.Handler{
			Backend: backend,
			IPAddr:  pktIP,
			Log:     log,
			Netboot: proxy.Netboot{
				IPXEBinServerTFTP: tftpIP,
				IPXEBinServerHTTP: httpBinaryURL,
				IPXEScriptURL:     ipxeScript,
				Enabled:           true,
			},
			OTELEnabled: true,
		}
		return dh, nil
	}

	return nil, errors.New("invalid dhcp mode")
}

// defaultLogger is zap logr implementation.
func defaultLogger(level string) logr.Logger {
	config := zap.NewProductionConfig()
	config.OutputPaths = []string{"stdout"}
	switch level {
	case "debug":
		config.Level = zap.NewAtomicLevelAt(zapcore.DebugLevel)
	default:
		config.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	}
	zapLogger, err := config.Build()
	if err != nil {
		panic(fmt.Sprintf("who watches the watchmen (%v)?", err))
	}

	return zapr.NewLogger(zapLogger)
}

func parseTrustedProxies(trustedProxies string) (result []string) {
	for _, cidr := range strings.Split(trustedProxies, ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, _, err := net.ParseCIDR(cidr)
		if err != nil {
			// Its not a cidr, but maybe its an IP
			if ip := net.ParseIP(cidr); ip != nil {
				if ip.To4() != nil {
					cidr += "/32"
				} else {
					cidr += "/128"
				}
			} else {
				// not an IP, panic
				panic("invalid ip cidr in TRUSTED_PROXIES cidr=" + cidr)
			}
		}
		result = append(result, cidr)
	}

	return result
}
