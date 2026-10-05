// Command reflectingpool scans storage pools, indexes what it finds and
// serves the result over an authenticated web API.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/IsaacFW/reflectingpool/internal/appdb"
	"github.com/IsaacFW/reflectingpool/internal/auth"
	"github.com/IsaacFW/reflectingpool/internal/core"
	"github.com/IsaacFW/reflectingpool/internal/scan"
	"github.com/IsaacFW/reflectingpool/internal/server"
)

var version = "dev"

const usage = `Reflecting Pool %s

Usage: reflectingpool <command>

  serve         run the web server (the container's default)
  scan          scan once, build the index and exit
                  -intensity aggressive|balanced|low   (default aggressive)
  bench [dir]   measure scan and query speed; changes nothing on the pool
                  -intensity aggressive|balanced|low   (default aggressive)
                  -for 5m    walk for that long and report the rate, to see
                             how much a scan slows other work on the pool
  doctor        report what this container can see and do
  reset-admin   delete the admin account so setup runs again
  version       print the version

Settings come from the environment:

  RP_ROOTS          directories to scan, comma-separated, e.g. /mnt/tank
  RP_DATA           where the index, account and TLS key are kept (default /data)
  RP_LISTEN         address to listen on (default :8443)
  RP_TLS_CERT       certificate file; with RP_TLS_KEY replaces the self-signed one
  RP_TLS_KEY        private key file
  RP_INSECURE_HTTP  1 to serve plain HTTP, for use behind a reverse proxy only
  RP_TRUST_PROXY    1 to trust X-Forwarded-For and X-Forwarded-Proto
  RP_READ_ONLY      1 to disable everything that changes files
  RP_EXCLUDE        extra directories to skip, comma-separated absolute paths
  RP_WORKERS       parallel directory walkers for an aggressive scan
                    (default: 4 per core, 8 to 32)
  RP_ZFS_LIST_FILE  output of the host script, when /dev/zfs is not passed in
                    (default <RP_DATA>/zfs-list.txt)
`

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe()
	case "scan":
		err = cmdScan(os.Args[2:])
	case "bench":
		err = cmdBench(os.Args[2:])
	case "doctor":
		err = cmdDoctor()
	case "reset-admin":
		err = cmdResetAdmin()
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Printf(usage, version)
	default:
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("error: %v", err)
	}
}

type settings struct {
	core         core.Config
	listen       string
	cert, key    string
	insecureHTTP bool
	trustProxy   bool
}

func envList(name string) []string {
	var out []string
	for _, part := range strings.Split(os.Getenv(name), ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envBool(name string) bool {
	v, _ := strconv.ParseBool(os.Getenv(name))
	return v
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func loadSettings() (settings, error) {
	s := settings{
		listen:       envOr("RP_LISTEN", ":8443"),
		cert:         os.Getenv("RP_TLS_CERT"),
		key:          os.Getenv("RP_TLS_KEY"),
		insecureHTTP: envBool("RP_INSECURE_HTTP"),
		trustProxy:   envBool("RP_TRUST_PROXY"),
	}
	s.core = core.Config{
		Roots:    envList("RP_ROOTS"),
		DataDir:  envOr("RP_DATA", "/data"),
		Exclude:  envList("RP_EXCLUDE"),
		ReadOnly: envBool("RP_READ_ONLY"),
	}
	if len(s.core.Roots) == 0 {
		return s, errors.New("RP_ROOTS is not set; set it to the pool mount points to scan, such as /mnt/tank")
	}
	for _, root := range s.core.Roots {
		// Unraid keeps Docker's image layers here by default: a very large
		// number of small files that say nothing about the user's data.
		s.core.Exclude = append(s.core.Exclude, filepath.Join(root, "system", "docker"))
	}
	for _, p := range s.core.Exclude {
		if !filepath.IsAbs(p) {
			return s, fmt.Errorf("RP_EXCLUDE: %q is not an absolute path", p)
		}
	}
	s.core.ZFSListFile = envOr("RP_ZFS_LIST_FILE", filepath.Join(s.core.DataDir, "zfs-list.txt"))
	if v := os.Getenv("RP_WORKERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return s, fmt.Errorf("RP_WORKERS: %q is not a positive number", v)
		}
		s.core.Workers = n
	}
	if (s.cert == "") != (s.key == "") {
		return s, errors.New("RP_TLS_CERT and RP_TLS_KEY must be set together")
	}
	return s, nil
}

// open prepares the data directory, the app database and the application.
func open(ctx context.Context, s settings) (*core.App, *auth.Service, func(), error) {
	if err := os.MkdirAll(s.core.DataDir, 0o700); err != nil {
		return nil, nil, nil, err
	}
	db, err := appdb.Open(filepath.Join(s.core.DataDir, "app.db"))
	if err != nil {
		return nil, nil, nil, err
	}
	app, err := core.New(ctx, s.core, db)
	if err != nil {
		db.Close()
		return nil, nil, nil, err
	}
	return app, auth.New(db, auth.DefaultParams), func() { app.Close(); db.Close() }, nil
}

func cmdServe() error {
	s, err := loadSettings()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	app, authSvc, closeAll, err := open(ctx, s)
	if err != nil {
		return err
	}
	defer closeAll()

	authSvc.OnSetupCode = func(code string) {
		log.Printf("No admin account exists yet. Open the web address and create one with this setup code:")
		log.Printf("")
		log.Printf("    %s", code)
		log.Printf("")
		log.Printf("The code is only shown here, in the container log.")
	}
	if _, err := authSvc.SetupRequired(); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              s.listen,
		Handler:           server.New(app, authSvc, server.Options{TLS: !s.insecureHTTP, TrustProxy: s.trustProxy, Version: version}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	log.Printf("Reflecting Pool %s; scanning %s; data in %s", version, strings.Join(app.Config().Roots, ", "), s.core.DataDir)
	if s.core.ReadOnly {
		log.Printf("read-only mode: nothing on the pool will be changed")
	}

	// The server never scans by itself unless the user has set a schedule.
	if app.IndexID() == "" {
		log.Printf("Nothing has been scanned yet. Start a scan from the web interface, or run: reflectingpool scan")
	}
	go app.RunScheduler(ctx)

	errc := make(chan error, 1)
	switch {
	case s.insecureHTTP:
		log.Printf("WARNING: serving plain HTTP on %s. Passwords are readable on the network unless a reverse proxy adds HTTPS.", s.listen)
		go func() { errc <- srv.ListenAndServe() }()
	case s.cert != "":
		log.Printf("serving HTTPS on %s with the certificate in %s", s.listen, s.cert)
		go func() { errc <- srv.ListenAndServeTLS(s.cert, s.key) }()
	default:
		cert, fingerprint, err := server.SelfSignedCert(filepath.Join(s.core.DataDir, "tls"), time.Now())
		if err != nil {
			return fmt.Errorf("creating the self-signed certificate: %w", err)
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		log.Printf("serving HTTPS on %s with a self-signed certificate; your browser will warn once.", s.listen)
		log.Printf("certificate SHA-256 fingerprint: %s", fingerprint)
		go func() { errc <- srv.ListenAndServeTLS("", "") }()
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Printf("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	intensityName := fs.String("intensity", "aggressive", "scan intensity: aggressive, balanced or low")
	if err := fs.Parse(args); err != nil {
		return err
	}
	intensity, err := scan.ParseIntensity(*intensityName)
	if err != nil {
		return err
	}
	s, err := loadSettings()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	app, _, closeAll, err := open(ctx, s)
	if err != nil {
		return err
	}
	defer closeAll()
	start := time.Now()
	info, err := app.Scan(ctx, intensity)
	if err != nil {
		return err
	}
	fmt.Printf("scanned %d files and %d folders in %s at %s intensity\n", info.Files, info.Dirs, time.Since(start).Round(time.Millisecond), intensity)
	fmt.Printf("apparent size %s, on disk %s, %d hardlinked files, %d errors\n", human(info.Size), human(info.Disk), info.Hardlinked, info.Errors)
	for _, w := range app.ScanStatus().Warnings {
		fmt.Printf("warning: %s\n", w)
	}
	return nil
}

func cmdResetAdmin() error {
	data := envOr("RP_DATA", "/data")
	db, err := appdb.Open(filepath.Join(data, "app.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := auth.New(db, auth.DefaultParams).Reset(); err != nil {
		return err
	}
	fmt.Println("The admin account and all sessions were deleted. Open the web address again: a new setup code will appear in the container log.")
	return nil
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
