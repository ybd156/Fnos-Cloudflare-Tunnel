// Command fncfd is the Cloudflare Tunnel for fnOS service.
//
// It replaces the Node service the app used to depend on: the package now ships
// a single static binary, so the fnOS app centre no longer has to install a
// Node.js runtime alongside it.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/applog"
	"github.com/ybd156/fnos-cloudflared/internal/cf"
	"github.com/ybd156/fnos-cloudflared/internal/config"
	"github.com/ybd156/fnos-cloudflared/internal/dnsroute"
	"github.com/ybd156/fnos-cloudflared/internal/fsx"
	"github.com/ybd156/fnos-cloudflared/internal/paths"
	"github.com/ybd156/fnos-cloudflared/internal/runner"
	"github.com/ybd156/fnos-cloudflared/internal/web"
)

func main() {
	p := paths.FromEnv()

	// Subcommands keep the lifecycle scripts free of JSON manipulation, which
	// used to be done by an inline Node script the package no longer has.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "config-set":
			runConfigSet(p)
			return
		case "--version", "-version", "version":
			fmt.Printf("fncfd %s\n", appVersion(p))
			return
		}
	}

	log := applog.New(p.AppLog)
	defer log.Close()

	// Best effort: a directory that cannot be created surfaces later with a
	// clearer message, and refusing to start would take the UI down with it.
	if err := p.EnsureDirs(); err != nil {
		log.Error("creating data directories:", err.Error())
	}
	// Rotate before the first write: at startup nothing else is appending, so
	// this is the one moment the check cannot race a concurrent writer.
	log.RotateAppLogNow()

	store := config.NewStore(p.ConfigFile, log)
	store.Load()

	cfRunner := cf.New(p, log)
	go func() {
		version := cfRunner.Version(true)
		if version == "" {
			version = "not found"
		}
		log.Info("cloudflared version:", version)
	}()

	dns := dnsroute.New(p, log, cfRunner)
	manager := runner.New(p, log, store, cfRunner, dns)
	server := web.New(p, log, store, cfRunner, dns, manager)
	handler := server.Handler()

	// The fnOS gateway reaches the app through a Unix socket inside the payload
	// directory; a loopback TCP port is kept as a fallback for the App Center's
	// port check and for local debugging.
	fsx.UnlinkQuiet(p.SocketPath)
	if listener, err := net.Listen("unix", p.SocketPath); err != nil {
		log.Error("failed to bind unix socket:", err.Error())
	} else {
		fsx.ChmodQuiet(p.SocketPath, 0o660)
		log.Info("listening on unix socket", p.SocketPath)
		go func() {
			if err := http.Serve(listener, handler); err != nil {
				log.Error("unix listener stopped:", err.Error())
			}
		}()
	}

	if p.TCPPort > 0 {
		addr := fmt.Sprintf("%s:%d", p.TCPHost, p.TCPPort)
		go func() {
			log.Info("listening on http://" + addr)
			if err := http.ListenAndServe(addr, handler); err != nil {
				log.Error("tcp listener stopped:", err.Error())
			}
		}()
	}

	if store.Settings().AutoStart {
		time.AfterFunc(1500*time.Millisecond, func() {
			for _, tunnel := range store.Tunnels() {
				if tunnel.Enabled && tunnel.AutoStart {
					log.Info("autostart tunnel", tunnel.ID, "("+tunnel.Name+")")
					if err := manager.Start(tunnel.ID); err != nil {
						log.Warn("autostart failed for", tunnel.ID+":", err.Error())
					}
				}
			}
		})
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	sig := <-signals
	log.Info("received", sig.String(), "shutting down")

	// Wait for the children to actually exit before leaving: exiting first hands
	// cloudflared over to init, where it keeps holding the tunnel connections
	// while the app can no longer see or stop it.
	manager.StopAll()
	manager.WaitIdle(10 * time.Second)
	log.Info("shutdown complete")
}

// runConfigSet applies the answers collected by the fnOS install/settings wizard
// to config.json and exits. cmd/config_callback calls this instead of running an
// inline script.
func runConfigSet(p paths.Paths) {
	if err := p.EnsureDirs(); err != nil {
		fmt.Fprintf(os.Stderr, "无法创建数据目录 %s: %v\n", p.PkgVar, err)
		os.Exit(1)
	}
	if err := config.ApplyWizardSettings(p.ConfigFile, os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "写入配置文件 %s 失败: %v\n", p.ConfigFile, err)
		os.Exit(1)
	}
}

// appVersion reports the version recorded in the package manifest.
func appVersion(p paths.Paths) string {
	log := applog.New(p.AppLog)
	defer log.Close()
	if v := cf.New(p, log).AppVersion(); v != "" {
		return v
	}
	return "unknown"
}
