// Command lamplighter is the Lamplighter service. The magpie-lamplighter
// plugin starts it from Magpie's plugin host; see docs/architecture.md.
//
// Usage:
//
//	lamplighter -listen 127.0.0.1:8317
//
// It serves the page, the API and the plugin host's tool calls on -listen,
// which takes no key and must be a loopback address; the optional public
// listener at public_listen in the configuration serves them behind
// admin_key and /mcp behind mcp_key. It keeps its configuration, state and
// history in the data directory, and reaches Magpie at MAGPIE_ADDR.
//
// Only one process uses a data directory: a second one waits for the
// instance lock before it reads anything there or listens on any port.
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
	"strings"
	"syscall"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/engine"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/magpie"
	"github.com/PosvdM/magpie-lamplighter/internal/server"
	"github.com/PosvdM/magpie-lamplighter/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// lockFile is the instance lock in the data directory.
const lockFile = "instance.lock"

// lockRetry is how often a process that waits for the instance lock tries
// again. Tests shorten it.
var lockRetry = time.Second

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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, *listen, *dataDir, magpie.FromEnv())
	stop()
	if err != nil {
		log.Fatal(err)
	}
}

// run serves until ctx ends. It takes the instance lock of the data
// directory first, waiting while another process holds it, and only then
// reads the configuration and the state and opens the listeners: a second
// process, such as a plugin host that Magpie starts for a command line
// like plugin update, must not answer on its port or act on files that
// the running process still writes.
func run(ctx context.Context, listen, dataDir string, h host.Host) error {
	if err := checkListen(listen); err != nil {
		return err
	}
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
	lock, err := lockDataDir(ctx, dataDir)
	if err != nil || lock == nil {
		return err
	}
	defer lock.Release()

	cfgStore, cfg, parseErr := config.Open(filepath.Join(dataDir, config.FileName))
	if cfgStore == nil {
		// The file exists but does not parse. Keep it for the user to fix
		// and run on the defaults without saving over it.
		return fmt.Errorf("%w; fix or remove the file", parseErr)
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	if !loopback(ln.Addr()) {
		ln.Close()
		return fmt.Errorf("-listen %s is bound to %s, not a loopback address", listen, ln.Addr())
	}
	e := engine.New(engine.Options{Host: h, Version: version, DataDir: dataDir})
	srv := server.New(cfgStore, cfg, nil, e, version)
	httpServer := newInternalServer(srv.Handler())
	srv.Start()
	log.Printf("%s listening on %s, data in %s", version, ln.Addr(), dataDir)

	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err = <-served:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	srv.Shutdown()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// newInternalServer returns the HTTP server of the internal listener. It
// sets no write timeout: a tool call takes up to 50 seconds and an action
// of the page waits up to 3 minutes for the background loop.
func newInternalServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
}

// lockDataDir takes the instance lock of dir, trying again every lockRetry
// while another process holds it. It returns a nil lock and no error when
// ctx ends first.
func lockDataDir(ctx context.Context, dir string) (*store.Lock, error) {
	path := filepath.Join(dir, lockFile)
	waiting := false
	for {
		lock, ok, err := store.TryLock(path)
		if err != nil {
			return nil, err
		}
		if ok {
			if waiting {
				log.Printf("took %s", path)
			}
			return lock, nil
		}
		if !waiting {
			log.Printf("another Lamplighter process holds %s; waiting for it to exit", path)
			waiting = true
		}
		timer := time.NewTimer(lockRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil
		case <-timer.C:
		}
	}
}

// checkListen refuses an internal listener address whose host is not
// localhost or a loopback IP address. The internal listener takes no key;
// access from elsewhere goes through public_listen and admin_key.
func checkListen(addr string) error {
	hostPart, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-listen %q: %w", addr, err)
	}
	if strings.EqualFold(hostPart, "localhost") {
		return nil
	}
	if ip := net.ParseIP(hostPart); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("-listen %q: the internal listener takes no key and must be on a loopback address (127.0.0.1, ::1 or localhost); use public_listen with admin_key for access from elsewhere", addr)
}

// loopback reports whether addr is bound to a loopback address only.
func loopback(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}
