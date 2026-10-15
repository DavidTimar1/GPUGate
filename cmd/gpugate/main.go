// Command gpugate is the GPUGate backend: one binary, one port, one copy. It serves the
// console page, the log, the settings, and the machine table; it rents and destroys machines
// on Verda and RunPod when Launch or Delete is pressed, destroys them when they are idle
// or cannot be served, and can move a project to a cheaper machine on its own.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/DavidTimar1/gpugate/internal/catalog"
	"github.com/DavidTimar1/gpugate/internal/console"
	"github.com/DavidTimar1/gpugate/internal/envfile"
	"github.com/DavidTimar1/gpugate/internal/login"
	"github.com/DavidTimar1/gpugate/internal/logring"
	"github.com/DavidTimar1/gpugate/internal/provider"
	"github.com/DavidTimar1/gpugate/internal/provider/runpod"
	"github.com/DavidTimar1/gpugate/internal/provider/verda"
	"github.com/DavidTimar1/gpugate/internal/store"
)

// defaultListen and defaultPort are what GPUGATE_LISTEN and GPUGATE_PORT fall back to: loopback
// only, so a fresh checkout is never reachable from another machine until its owner says so.
const (
	defaultListen = "127.0.0.1"
	defaultPort   = "8719"
)

// dbPath is the SQLite file, untracked working state.
const dbPath = ".state/gpugate.db"

// modelsPath is the committed model catalog, like the other paths in this file.
const modelsPath = "catalog/models.json"

// envPath is the repository's own credentials file, read and never sourced.
const envPath = ".env"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gpugate: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	// make-key reads nothing and touches no file: it prints one random key and stops.
	if len(os.Args) > 1 && os.Args[1] == "make-key" {
		if len(os.Args) != 2 {
			return fmt.Errorf("make-key takes no arguments\n%s", usage)
		}
		return commandMakeKey()
	}

	// Read, never sourced: a name already exported in the environment wins over the file.
	// No credential value is ever printed by this call or by anything downstream of it.
	if err := envfile.Load(envPath); err != nil {
		return fmt.Errorf("read %s: %w", envPath, err)
	}

	if len(os.Args) > 1 {
		return runCommand(os.Args[1:])
	}

	addrs, err := listenAddrs()
	if err != nil {
		return err
	}

	// Every setting that can stop the start is checked here, before the database is opened or
	// .state is created, so a bad setting leaves nothing behind.
	accessKey := strings.TrimSpace(os.Getenv("GPUGATE_ACCESS_KEY"))
	if accessKey != "" && len(accessKey) < minAccessKeyLength {
		return fmt.Errorf("GPUGATE_ACCESS_KEY is shorter than %d characters; make one with ./dist/gpugate make-key", minAccessKeyLength)
	}

	uiDir, err := findUIDir()
	if err != nil {
		return err
	}

	models, err := catalog.Load(modelsPath)
	if err != nil {
		return fmt.Errorf("load the model catalog: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dbPath), err)
	}
	// The folder holds the database, the keys and the password hash: closed to other accounts,
	// whatever mode an older copy of it was created with.
	if err := os.Chmod(filepath.Dir(dbPath), 0o700); err != nil {
		return fmt.Errorf("restrict %s: %w", filepath.Dir(dbPath), err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer st.Close()

	log := logring.New()
	log.Add("server", "GPUGate is starting")

	// Every provider client writes an outbound call's slow-or-failed line into the log under the
	// "providers" source, the one the console uses for a provider call that could not be asked.
	providers := buildProviders(func(text string) { log.Add("providers", text) })

	// Read once, here, alongside the other credentials: no provider package reads the
	// environment for itself. HF_API_KEY first, then HF_TOKEN; the first non-empty wins.
	// "" means no token is configured: every download then proceeds unauthenticated. Never
	// printed.
	hfToken := os.Getenv("HF_API_KEY")
	if hfToken == "" {
		hfToken = os.Getenv("HF_TOKEN")
	}

	// The login wraps the whole route table. GPUGATE_ACCESS_KEY is optional: unset, the fixed
	// address and the project list stay open to whoever can reach a listening address.
	cookieSecure := os.Getenv("GPUGATE_COOKIE_SECURE") == "1"
	gate := login.New(login.HashPath, accessKey, allowedHosts(), cookieSecure, func(text string) { log.Add("server", text) })
	handler := gate.Wrap(console.New(st, log, uiDir, models, providers, hfToken))
	if accessKey == "" {
		line := "GPUGATE_ACCESS_KEY is not set: /p/ and GET /api/projects are open to anyone who can reach a listening address"
		fmt.Println(line)
		log.Add("server", line)
	}
	if _, err := os.Stat(login.HashPath); err != nil {
		line := "no console password is set: run ./dist/gpugate set-password; until then the console answers nothing but the login page"
		fmt.Println(line)
		log.Add("server", line)
	}

	// These addresses and no others, and stopping rather than moving to a free port when
	// one of them is taken. net.Listen fails plainly with "address already in use" for that case.
	// Never 0.0.0.0 or :: (listenAddrs refuses them): only the addresses GPUGATE_LISTEN names.
	listeners := make([]net.Listener, 0, len(addrs))
	for _, addr := range addrs {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf(
				"could not listen on %s: %w (is another copy of gpugate already running? "+
					"this program never moves to a different port)", addr, err,
			)
		}
		listeners = append(listeners, listener)
	}

	// A header read timeout and no write timeout: the fixed address streams answers for as long as
	// a model keeps generating.
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, len(listeners))
	for _, listener := range listeners {
		go func() {
			serveErr <- server.Serve(listener)
		}()
	}

	for _, addr := range addrs {
		host, _, _ := net.SplitHostPort(addr)
		if !trustedAddress(net.ParseIP(host)) {
			line := "WARNING: listening on " + addr + ", which is not loopback or a Tailscale address; the login travels as plain HTTP there, so anyone on that network can read it"
			fmt.Println(line)
			log.Add("server", line)
		}
		line := "GPUGate is listening on http://" + addr + "/console"
		fmt.Println(line)
		log.Add("server", line)
	}
	fmt.Println("Ctrl-C to stop.")

	select {
	case <-ctx.Done():
		log.Add("server", "the server has stopped")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down cleanly: %w", err)
		}
		return nil
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("the server stopped unexpectedly: %w", err)
		}
		return nil
	}
}

// buildProviders makes the provider clients from the environment. Credential values are
// read here, once, and no provider package reads the environment for itself. logf receives a
// provider call's own slow-or-failed line.
func buildProviders(logf provider.LogFunc) []provider.Provider {
	return []provider.Provider{
		verda.New(os.Getenv("VERDA_MAIN_CLIENT_ID"), os.Getenv("VERDA_MAIN_CLIENT_SECRET"), logf),
		runpod.New(os.Getenv("RUNPOD_API_GPU_GATE"), logf),
	}
}

// listenAddrs reads GPUGATE_LISTEN (a comma-separated list of IP literals) and GPUGATE_PORT and
// returns the host:port pairs to bind, exactly those and no others. A hostname, 0.0.0.0 and ::
// are refused: this console is reachable only on addresses its owner named.
func listenAddrs() ([]string, error) {
	listen := strings.TrimSpace(os.Getenv("GPUGATE_LISTEN"))
	if listen == "" {
		listen = defaultListen
	}
	port := strings.TrimSpace(os.Getenv("GPUGATE_PORT"))
	if port == "" {
		port = defaultPort
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("GPUGATE_PORT must be a whole number from 1 to 65535")
	}
	var addrs []string
	for _, item := range strings.Split(listen, ",") {
		item = strings.TrimSpace(item)
		ip := net.ParseIP(item)
		if ip == nil {
			return nil, fmt.Errorf("GPUGATE_LISTEN names %q, which is not an IP address; give a comma-separated list of IP addresses such as 127.0.0.1", item)
		}
		if ip.IsUnspecified() {
			return nil, fmt.Errorf("GPUGATE_LISTEN names %q, which means every interface; this program never listens on every interface, so name the addresses you want", item)
		}
		addrs = append(addrs, net.JoinHostPort(ip.String(), port))
	}
	return addrs, nil
}

// minAccessKeyLength is the fewest characters a GPUGATE_ACCESS_KEY may have; make-key prints one
// of 64.
const minAccessKeyLength = 32

// commandMakeKey prints one random access key and nothing else.
func commandMakeKey() error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("mint a key: %w", err)
	}
	fmt.Println(hex.EncodeToString(raw))
	return nil
}

// Tailscale hands out addresses from 100.64.0.0/10 (IPv4) and fd7a:115c:a1e0::/48 (IPv6). The
// IPv4 range is also carrier-grade NAT space, so an address in it is not always a Tailscale one.
var (
	_, vpnV4, _ = net.ParseCIDR("100.64.0.0/10")
	_, vpnV6, _ = net.ParseCIDR("fd7a:115c:a1e0::/48")
)

// trustedAddress reports whether ip is loopback or in Tailscale's address ranges, the addresses
// the plain-HTTP login is considered safe on.
func trustedAddress(ip net.IP) bool {
	return ip != nil && (ip.IsLoopback() || vpnV4.Contains(ip) || vpnV6.Contains(ip))
}

// allowedHosts is every name a request may be addressed to: each listen address, localhost, and
// the names in GPUGATE_HOSTNAMES (comma-separated, optional).
func allowedHosts() []string {
	hosts := []string{"localhost"}
	addrs, err := listenAddrs()
	if err == nil {
		for _, addr := range addrs {
			host, _, _ := net.SplitHostPort(addr)
			hosts = append(hosts, host)
		}
	}
	for _, name := range strings.Split(os.Getenv("GPUGATE_HOSTNAMES"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			hosts = append(hosts, name)
		}
	}
	return hosts
}

// findUIDir locates the ui/ directory this binary serves, relative to the current working
// directory first, and next to the binary itself when it is run from somewhere else.
func findUIDir() (string, error) {
	candidates := []string{"ui"}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "ui"))
	}
	for _, dir := range candidates {
		if info, err := os.Stat(filepath.Join(dir, "index.html")); err == nil && !info.IsDir() {
			return dir, nil
		}
	}
	return "", fmt.Errorf("could not find ui/index.html under any of %v; run gpugate from the repository root", candidates)
}
