// Command lamplighter is the Lamplighter service. The magpie-lamplighter
// plugin starts it from Magpie's plugin host; see docs/architecture.md.
//
// Usage:
//
//	lamplighter -listen 127.0.0.1:8317
//
// It serves the page, the API and the plugin host's tool calls on -listen,
// which takes no key and belongs on loopback; the optional public listener
// at public_listen in the configuration serves them behind admin_key and
// /mcp behind mcp_key. It keeps its configuration, state and history in the
// data directory, and reaches Magpie at MAGPIE_ADDR.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/engine"
	"github.com/PosvdM/magpie-lamplighter/internal/magpie"
	"github.com/PosvdM/magpie-lamplighter/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	listen := flag.String("listen", "127.0.0.1:8317", "internal listener address, on loopback")
	dataDir := flag.String("data-dir", "", "data directory (default: LAMPLIGHTER_DATA_DIR, else lamplighter under XDG_STATE_HOME)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("lamplighter: ")
	if err := run(*listen, *dataDir); err != nil {
		log.Fatal(err)
	}
}

func run(listen, dataDir string) error {
	if dataDir == "" {
		dir, err := config.DataDir()
		if err != nil {
			return err
		}
		dataDir = dir
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	store, cfg, parseErr := config.Open(filepath.Join(dataDir, config.FileName))
	if store == nil {
		// The file exists but does not parse. Keep it for the user to fix
		// and run on the defaults without saving over it.
		return fmt.Errorf("%w; fix or remove the file", parseErr)
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	if !loopback(ln.Addr()) {
		log.Printf("warning: %s is not a loopback address; the internal listener takes no key, use public_listen with admin_key instead", ln.Addr())
	}
	e := engine.New(engine.Options{Host: magpie.FromEnv(), Version: version, DataDir: dataDir})
	srv := server.New(store, cfg, nil, e, version)
	httpServer := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	srv.Start()
	log.Printf("%s listening on %s, data in %s", version, ln.Addr(), dataDir)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(ln) }()
	select {
	case <-stop:
	case err = <-served:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
	srv.Shutdown()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// loopback reports whether addr is bound to a loopback address only.
func loopback(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}
