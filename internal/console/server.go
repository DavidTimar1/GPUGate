// Package console is the HTTP server: static ui/, and the JSON and HTML-fragment calls the
// page makes. It is the only package that knows HTTP shapes: the machine table and the model
// dropdown's recalculation (read-only), the running strip's live poll and its own watcher
// (read-only), Delete, and Launch. Launch and Delete are the only two calls that spend or end
// real money, and the auto-move (mover.go) makes the same two calls for a project with its switch
// on; nothing here reconfigures a machine once it exists.
package console

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DavidTimar1/gpugate/internal/catalog"
	"github.com/DavidTimar1/gpugate/internal/logring"
	"github.com/DavidTimar1/gpugate/internal/provider"
	"github.com/DavidTimar1/gpugate/internal/sshtunnel"
	"github.com/DavidTimar1/gpugate/internal/store"
)

// providerCallTimeout bounds how long the table waits on one provider. A provider that hangs
// must not hold the page: it is reported as unreachable rather than left pending. The one-hour
// launch clock governs the launch path, not a read-only list call, so this stays short.
const providerCallTimeout = 30 * time.Second

// projectProxyPrefix is the fixed address's path prefix, before the project slug: a request path
// is "/p/<slug>/...". One handler on the wildcard reads the slug out of the path.
const projectProxyPrefix = "/p/"

// maxProxyBodyBytes caps how much of a request body handleProjectProxy will read into memory.
// Every body is held whole: it is what names the model the request goes to. Above this the
// request is refused with 413 rather than forwarded, because nothing can be routed from a body
// that was never read.
const maxProxyBodyBytes = 64 << 20 // 64 MB

// Server holds what the HTTP handlers need: the store, the log ring, the directory ui/ was
// found in, the model catalog, and the provider clients.
type Server struct {
	store     *store.Store
	log       *logring.Ring
	uiDir     string
	models    []catalog.Model
	byName    map[string]catalog.Model
	providers []provider.Provider
	// hfToken is the user's Hugging Face token (HF_API_KEY, falling back to HF_TOKEN), read once
	// by cmd/gpugate/main.go and carried onto every launch's provider.ServeOptions. "" means none
	// is configured. Never logged, never put in a claim record.
	hfToken string

	// cacheMu guards the last successful machine-table read, which Recalculate judges again
	// without asking any provider anything: recalculating costs nothing.
	cacheMu    sync.Mutex
	cachedFrom []provider.Shape
	haveCache  bool

	// runMu guards the running strip's cache: what the live poll last heard from each provider. A
	// failed or skipped poll never overwrites a provider's last successful answer with silence;
	// it is recorded as its own outcome instead.
	runMu    sync.Mutex
	runState map[string]*providerRunState

	// destroyMu guards destroyNotices: one line per machine currently being destroyed or whose
	// destroy could not be confirmed, keyed by "<provider_key>/<provider_id>". The strip reads it
	// on every render so a refusal, or an exhausted retry, stays visible on that machine's row
	// rather than only in the log.
	destroyMu      sync.Mutex
	destroyNotices map[string]string

	// abandonMu guards abandonMisses: how many consecutive periodic passes past an open claim's
	// one-hour abandon ceiling have found no machine for it, keyed by claim id. The watcher only
	// gives up once this reaches claimAbandonMissesNeeded, because a single miss can be a live
	// poll whose request began before the machine appeared, not proof it never did.
	abandonMu     sync.Mutex
	abandonMisses map[int64]int

	// proxyClient carries every call the fixed address (/p/<project>) and the readiness
	// poll make to a serving machine's address. It carries no fixed timeout: a streamed chat
	// completion may run long, and each call is instead bound to its own request's context (the
	// readiness poll's short one, or the proxied request's own).
	proxyClient *http.Client

	// startedAt is this process's start time, before the first pollRunningOnce runs. It is the
	// last resort for the inactivity sweep's quiet-since (see quietSince): a restart must never
	// destroy a working machine on a clock the program has no evidence about.
	startedAt time.Time

	// lastOutputMu guards lastOutputAt (the fixed address's in-memory "last output" clock, keyed
	// by machineRef so both providers get their own clock) and flushedOutputAt (what was last
	// written to the store from it). The proxy's hot path only touches lastOutputAt in memory;
	// flushLastOutputAt writes a changed entry to the database once per live-poll pass.
	lastOutputMu    sync.Mutex
	lastOutputAt    map[machineRef]time.Time
	flushedOutputAt map[machineRef]time.Time

	// tokenMu guards tokenDeltas: each machine's tokens in, out and cached, read out of every
	// answer the fixed address has forwarded for it since the last live-poll flush. The proxy's
	// hot path only adds to this in memory (recordUsage); flushTokenDeltas adds a changed entry to
	// the store once per live-poll pass and clears what it flushed. It is the pattern
	// lastOutputAt and flushedOutputAt use above, except these are deltas to add rather than an
	// absolute clock to overwrite.
	tokenMu     sync.Mutex
	tokenDeltas map[machineRef]tokenDelta

	// roleOrderMu guards roleOrderLogged: which machines (keyed by "<provider_key>/<provider_id>")
	// have already earned the one log line naming the role order of the first /v1/messages body
	// they received (roles only, never content).
	roleOrderMu     sync.Mutex
	roleOrderLogged map[string]bool

	// tunnelMu guards tunnels (the SSH tunnel to each Verda machine's loopback, keyed by
	// machineRef) and noKeyLogged (so a missing key file earns one log line per machine, not one
	// every live-poll pass). sshKey is loaded once by New, nil with one logged reason when the
	// file could not be read or parsed.
	tunnelMu    sync.Mutex
	tunnels     map[machineRef]*sshtunnel.Tunnel
	sshKey      *sshtunnel.Key
	noKeyLogged map[machineRef]bool
	// tunnelMisses counts consecutive successful lists that lacked a machine holding a tunnel.
	tunnelMisses map[machineRef]int

	// readinessPollersMu guards readinessPollers: which claim ids currently have an active
	// pollServingReadiness goroutine. startReadinessPoll's callers (reconcileClaims,
	// resumeServingPolls) run inside pollRunningOnce, which has no lock against running
	// concurrently with itself: runLaunch's "ask for one now" call can race the periodic ticker's
	// call, and both can observe the same claim not yet confirmed. This map makes a second start
	// for a claim already being polled a no-op, so one claim never gets two readiness pollers.
	readinessPollersMu sync.Mutex
	readinessPollers   map[int64]bool

	// inflightMu guards inflight: how many requests the fixed address is forwarding to each machine
	// right now. The auto-move reads it to destroy a machine it has switched away from only once
	// nothing is still being answered by it (a destroy cuts a stream).
	inflightMu sync.Mutex
	inflight   map[machineRef]int

	// moverMu guards moverRunning: which move ids have a runMove goroutine now, so a second start
	// for the same move is a no-op, the way readinessPollers does it for a claim.
	moverMu      sync.Mutex
	moverRunning map[int64]bool
	// moveSkipLogged is which claims already earned their one "never moved" line (also under
	// moverMu).
	moveSkipLogged map[int64]bool
	// moveNoCheaperLogged is the last "no cheaper machine" reason written per claim, so an
	// unchanged reason is not written again on every look (also under moverMu).
	moveNoCheaperLogged map[int64]string
	// moverPassMu serializes mover passes: one project never gets two moves started.
	moverPassMu sync.Mutex
}

// machineRef addresses one machine on one provider: a claim's provider_key and provider_id pair,
// so the inactivity clock and the token deltas each get an entry per provider.
type machineRef struct {
	providerKey string
	providerID  string
}

// tokenDelta is one machine's not-yet-flushed token counts: tokens in (prompt tokens not read
// from cache), tokens out, and tokens read from cache.
type tokenDelta struct {
	in, out, cached int
}

// providerRunState is what the live poll last found for one provider: either a fresh list of
// machines, or the reason it could not ask.
type providerRunState struct {
	key         string
	displayName string
	ok          bool // true only for a fresh, successful answer this poll
	machines    []provider.RunningMachine
	// lastMachines is the newest successful list this provider gave, kept through failed passes.
	// Only the fixed address and the project list read it (lastKnownMachines); everything that
	// destroys, sweeps, confirms or moves reads machines under ok alone.
	lastMachines []provider.RunningMachine
	errText      string // set when !ok
	// askedAt is when this answer's ListMachines call was ASKED FOR: the moment the request began,
	// not when it landed. A request that began before a machine was confirmed can still land
	// after the confirmation and correctly carry no such machine, so only an answer whose askedAt
	// is after a claim's confirm time is trusted to say that claim's machine has vanished.
	askedAt time.Time
	// newlyConfirmed is true when this pass confirmed a claim's machine as running. The mover is
	// asked to look once this answer is stored, because a launch on an auto-move project can be
	// moved while it still loads.
	newlyConfirmed bool
}

// New builds the HTTP handler for the whole console: the static ui/ files, /console as the page
// itself, and the API calls.
func New(st *store.Store, log *logring.Ring, uiDir string, models []catalog.Model, providers []provider.Provider, hfToken string) http.Handler {
	byName := make(map[string]catalog.Model, len(models))
	for _, m := range models {
		byName[m.Name] = m
	}
	// The SSH key every Verda tunnel signs with, loaded once: a missing or unparseable file means
	// no Verda machine can ever be reached, reported once here rather than once per machine
	// (ensureTunnel's noKeyLogged dedupes that path too).
	sshKey, sshKeyErr := sshtunnel.LoadKey()
	if sshKeyErr != nil {
		log.Add("server", "no SSH key could be loaded from "+sshtunnel.PrivateKeyPath+": "+sshKeyErr.Error())
		sshKey = nil
	}
	s := &Server{
		store: st, log: log, uiDir: uiDir, models: models, byName: byName, providers: providers,
		hfToken:  hfToken,
		runState: map[string]*providerRunState{}, destroyNotices: map[string]string{},
		abandonMisses:    map[int64]int{},
		proxyClient:      &http.Client{Transport: provider.NewUserAgentTransport(http.DefaultTransport)},
		startedAt:        time.Now(),
		lastOutputAt:     map[machineRef]time.Time{},
		flushedOutputAt:  map[machineRef]time.Time{},
		tokenDeltas:      map[machineRef]tokenDelta{},
		roleOrderLogged:  map[string]bool{},
		tunnels:          map[machineRef]*sshtunnel.Tunnel{},
		sshKey:           sshKey,
		noKeyLogged:      map[machineRef]bool{},
		tunnelMisses:     map[machineRef]int{},
		readinessPollers: map[int64]bool{},
		inflight:         map[machineRef]int{},
		moverRunning:     map[int64]bool{},
	}
	mux := http.NewServeMux()

	// The page's files answer with Cache-Control: no-cache, so a browser revalidates every one of
	// them on every load instead of deciding for itself how long to keep it. Without a
	// Cache-Control header a browser is free to guess a lifetime from Last-Modified, which is how
	// a changed page can stay invisible to the user. no-cache still allows a 304, so nothing is
	// re-sent when nothing changed.
	mux.Handle("/", noBrowserCache(http.HandlerFunc(s.handleStatic)))
	mux.HandleFunc("/console", s.handleConsolePage)

	mux.HandleFunc("/api/log", s.handleLog)
	mux.HandleFunc("POST /api/shapes/{shape_code}/permission", s.handleShapePermission)
	mux.HandleFunc("/api/machines", s.handleMachinesAndModels)
	mux.HandleFunc("/api/models", s.handleRecalculate)
	mux.HandleFunc("/api/running", s.handleRunning)
	mux.HandleFunc("GET /api/claims/{id}", s.handleClaimState)
	mux.HandleFunc("GET /api/projects", s.handleProjects)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("POST /api/settings/sessions-since", s.handleSessionsSince)
	mux.HandleFunc("POST /api/projects", s.handleCreateProject)
	mux.HandleFunc("DELETE /api/projects/{slug}", s.handleDeleteProject)
	mux.HandleFunc("POST /api/projects/{slug}/auto-move", s.handleProjectAutoMove)
	mux.HandleFunc("POST /api/projects/{slug}/price-cap", s.handleProjectPriceCap)
	mux.HandleFunc("POST /api/machines/{provider}/{id}/destroy", s.handleDestroy)
	mux.HandleFunc("POST /api/machines/{provider}/{id}/idle-minutes", s.handleIdleMinutes)
	mux.HandleFunc("GET /api/launch-offer", s.handleLaunchOffer)
	mux.HandleFunc("POST /api/launch", s.handleLaunch)
	mux.HandleFunc("GET /api/models/{name}/settings", s.handleModelSettingsGet)
	mux.HandleFunc("PUT /api/models/{name}/settings", s.handleModelSettingsPut)
	mux.HandleFunc(projectProxyPrefix, s.handleProjectProxy)

	// The first pass runs to completion before the listener accepts, so the strip never draws
	// from our notes alone on a fresh start. It then keeps polling on its own timer for the life
	// of the process.
	s.pollRunningOnce(context.Background())
	go s.pollRunningForever()

	// A machine that outlives GPUGate keeps running, and starting GPUGate again finds it and
	// brings its record up to date. A claim still 'loading' from before this process started is
	// exactly that: its readiness poll died with the old process, so it is resumed here rather
	// than left to wait forever.
	s.resumeServingPolls()

	// A destroy_intent row left open by a process that died mid-retry is resumed here rather than
	// left to bill forever with nothing retrying or reporting it.
	s.resumeDestroys()

	// A move still under way when the process died is finished by this one, and the mover then
	// looks for new ones on its own timer.
	s.resumeMoves()
	go s.moverForever()
	s.lookForCheaperNow()

	return mux
}

// handleStatic serves the few files of ui/ the page needs and nothing else: the page itself at
// "/", the three tab icons (idle, running, warning), and the files under dist/ and vendor/. It never serves a directory listing,
// and never the build script, the package file or the readme that sit beside them. The login
// wraps this like every other route.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeRefusal(w, http.StatusMethodNotAllowed, "GET is the only method the page's files answer.")
		return
	}
	p := r.URL.Path
	var file string
	switch {
	case p == "/":
		file = "index.html"
	case p == "/favicon.svg" || p == "/favicon-running.svg" || p == "/favicon-warning.svg":
		file = strings.TrimPrefix(p, "/")
	case strings.HasPrefix(p, "/dist/") || strings.HasPrefix(p, "/vendor/"):
		file = strings.TrimPrefix(path.Clean(p), "/")
	default:
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(s.uiDir, filepath.FromSlash(file))
	if info, err := os.Stat(full); err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, full)
}

// handleConsolePage serves ui/index.html at /console: the console page is reached here, while
// /p/<project> is the fixed address a client talks to.
func (s *Server) handleConsolePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeRefusal(w, http.StatusMethodNotAllowed, "GET is the only method /console answers.")
		return
	}
	// Same reason as the cache note in New: a browser must ask before it reuses the page.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, s.uiDir+"/index.html")
}

// logLine is one line of GET /api/log's response shape, as ui/src/app.js reads it.
type logLine struct {
	Sequence int64  `json:"sequence"`
	At       string `json:"at"`
	Source   string `json:"source"`
	Text     string `json:"text"`
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeRefusal(w, http.StatusMethodNotAllowed, "GET is the only method /api/log answers.")
		return
	}
	after := int64(0)
	if v := r.URL.Query().Get("after"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeRefusal(w, http.StatusBadRequest, "after must be a whole number.")
			return
		}
		after = parsed
	}
	lines, newestSequence, ringHolds := s.log.After(after)
	out := make([]logLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, logLine{Sequence: l.Sequence, At: l.At, Source: l.Source, Text: l.Text})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lines":           out,
		"newest_sequence": newestSequence,
		"ring_holds":      ringHolds,
	})
}

// providerLabel turns a provider key back into the word the page uses ("Verda", "RunPod"),
// without the running-strip code needing a Server to look it up.
func providerLabel(key string) string {
	switch key {
	case "verda":
		return "Verda"
	case "runpod":
		return "RunPod"
	default:
		return key
	}
}

// writeRefusal answers a non-2xx with the one-sentence shape ui/src/app.js reads: {"detail":
// "<one sentence>"}, never a list of objects.
func writeRefusal(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]any{"detail": detail})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// noBrowserCache wraps a handler so every answer carries Cache-Control: no-cache. See the note
// in New.
func noBrowserCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}
