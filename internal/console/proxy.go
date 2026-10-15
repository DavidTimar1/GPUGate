package console

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/DavidTimar1/gpugate/internal/podkey"
	"github.com/DavidTimar1/gpugate/internal/store"
)

// servingTarget is where the fixed address forwards to: a serving machine's address and its key.
// Never logged, never put on the page, only used to build one outgoing request.
type servingTarget struct {
	providerKey string // "runpod" or "verda", for the key file and the per-call log line
	providerID  string // this machine's provider id, for the inactivity timeout's clock
	baseURL     string // RunningMachine.ServingBaseURL, no trailing path
	apiKey      string // this machine's random key, read from its key file
	format      string // the served model's catalog format ("gguf" means llama.cpp, else vLLM)
}

// nothingServingSentence is what the fixed address answers when nothing is serving anything at
// all, as opposed to serving something other than what the request named.
const nothingServingSentence = "Nothing is serving a model right now. Launch a model first."

// servingPod is one machine serving a model right now: the claim that asked for it, the machine
// it got, and the name a request's own "model" field must carry to land on it.
type servingPod struct {
	claim       store.Claim
	providerKey string
	providerID  string
	servedModel string
	format      string // the catalog format this machine serves ("gguf" means llama.cpp, else vLLM)
}

// servingPods answers every machine, on either provider, serving a model right now for project,
// oldest claim first, so two can serve at once and a caller can pick between them by name (never
// a machine launched for a different project).
//
// A claim keeps serving_state 'serving' after its machine is destroyed, so the claim record alone
// is never proof: a claim is listed only while a fresh live-poll answer still carries its
// machine.
//
// live says which machines count: lastKnownMachines for the fixed address and the project list,
// which keep serving through a provider blip, liveMachines for the mover and the delete refusal,
// which act on fresh answers only.
func (s *Server) servingPods(project string, live map[string]map[string]bool) ([]servingPod, error) {
	claims, err := s.store.ServingClaims(project)
	if err != nil {
		return nil, err
	}
	var pods []servingPod
	for _, c := range claims {
		if c.ProviderID == nil || !live[c.ProviderKey][*c.ProviderID] {
			continue
		}
		// The served name is the catalog's repository name, which is what vLLM is given as
		// --model and what llama.cpp is given as --alias, so it is the name the machine answers
		// to.
		served := s.byName[c.ModelName].HFRepo
		if served == "" {
			served = c.ModelName
		}
		pods = append(pods, servingPod{claim: c, providerKey: c.ProviderKey, providerID: *c.ProviderID, servedModel: served, format: s.byName[c.ModelName].Format})
	}
	return pods, nil
}

// projectActivePod answers the hostname and model name of a LIVE machine, loading or serving,
// launched for project, for handleDeleteProject's refusal. "Live" is judged as the rest of this
// program judges it, never by a claim's stored mark alone: `serving_state` stays 'serving' (or
// 'loading') forever once its machine is destroyed, because nothing clears it. So the serving
// half reuses servingPods(project), whose fresh-live-poll gate already excludes a destroyed
// machine, and the loading half applies the same gate to ClaimsAwaitingServing's rows, filtered
// to project here because that store function has no project parameter (resumeServingPolls reads
// it account-wide). found is false when project has no live machine either way.
func (s *Server) projectActivePod(project string) (hostname, modelName string, found bool, err error) {
	pods, err := s.servingPods(project, s.lastKnownMachines())
	if err != nil {
		return "", "", false, err
	}
	if len(pods) > 0 {
		return pods[0].claim.Hostname, pods[0].claim.ModelName, true, nil
	}
	loading, err := s.store.ClaimsAwaitingServing()
	if err != nil {
		return "", "", false, err
	}
	live := s.lastKnownMachines()
	for _, c := range loading {
		if c.Project != project || c.ProviderID == nil || !live[c.ProviderKey][*c.ProviderID] {
			continue
		}
		return c.Hostname, c.ModelName, true, nil
	}
	return "", "", false, nil
}

// servingModelNames lists what is serving right now, each name once, for a refusal a caller
// reads. The names come from the catalog, so they are ASCII.
func servingModelNames(pods []servingPod) string {
	seen := map[string]bool{}
	names := []string{}
	for _, p := range pods {
		if seen[p.servedModel] {
			continue
		}
		seen[p.servedModel] = true
		names = append(names, p.servedModel)
	}
	return strings.Join(names, ", ")
}

// proxyTarget picks the machine one /p/<project> request goes to, from the body already read,
// among only that project's serving machines.
//
// A request carrying a body names its model in the body's top-level "model" field, the one place
// both the OpenAI shape (/v1/chat/completions) and the Anthropic shape (/v1/messages) put it,
// and goes to the machine serving that name. Where two machines serve the same name the newest
// claim takes it, and nothing on the page says which; an older one takes it only while the newer
// is unreachable (reachableTarget), which is what lets an auto-move switch without a gap. A
// request with no body, GET /v1/models and the like, names nothing and goes to the project's
// newest serving machine, with the same fallback among the machines serving that model.
//
// On a refusal it answers a nil target with the status and the one sentence to send back. The
// caller's model name is never echoed into that sentence: it is untrusted text, and this
// program writes ASCII only.
func (s *Server) proxyTarget(project string, body []byte) (*servingTarget, int, string) {
	pods, err := s.servingPods(project, s.lastKnownMachines())
	if err != nil {
		return nil, http.StatusInternalServerError, "Could not read what is serving."
	}
	if len(pods) == 0 {
		return nil, http.StatusServiceUnavailable, nothingServingSentence
	}
	var candidates []servingPod // newest first
	if len(body) == 0 {
		newest := pods[len(pods)-1]
		for i := len(pods) - 1; i >= 0; i-- {
			if pods[i].servedModel == newest.servedModel {
				candidates = append(candidates, pods[i])
			}
		}
	} else {
		var asked struct {
			Model string `json:"model"`
		}
		// A body that is not a JSON object, or one with no "model", names nothing, and the refusal
		// below says what is serving rather than guessing at a pod.
		_ = json.Unmarshal(body, &asked)
		if asked.Model != "" {
			for i := len(pods) - 1; i >= 0; i-- { // pods are oldest first
				if pods[i].servedModel == asked.Model {
					candidates = append(candidates, pods[i])
				}
			}
		}
		if len(candidates) == 0 {
			return nil, http.StatusBadRequest, "This request names no model that is serving. Serving right now: " + servingModelNames(pods) + "."
		}
	}
	return s.reachableTarget(candidates)
}

// reachableTarget answers the first of candidates (newest first) that can be reached right now:
// the newest successful live-poll list still carries it (kept through a provider blip), its
// serving address is known and usable (on Verda, its tunnel connected), and its key is on file.
// A machine already being destroyed is never chosen. Where none can be reached it answers the
// failure of the newest.
func (s *Server) reachableTarget(candidates []servingPod) (*servingTarget, int, string) {
	firstStatus, firstDetail := http.StatusServiceUnavailable, nothingServingSentence
	for i, pod := range candidates {
		if _, destroying := s.destroyNotice(pod.providerKey, pod.providerID); destroying {
			continue
		}
		fail := func(status int, detail string) {
			if i == 0 {
				firstStatus, firstDetail = status, detail
			}
		}
		m, have := s.lastKnownMachineByID(pod.providerKey, pod.providerID)
		if !have {
			continue
		}
		baseURL, stage := s.servingBaseURL(m)
		if baseURL == "" || stage != "" {
			continue
		}
		key, err := podkey.Read(pod.providerKey, pod.providerID)
		if err != nil {
			fail(http.StatusInternalServerError, "No key on file for the machine serving "+pod.servedModel+".")
			continue
		}
		return &servingTarget{
			providerKey: pod.providerKey,
			providerID:  pod.providerID,
			baseURL:     baseURL,
			apiKey:      key,
			format:      pod.format,
		}, 0, ""
	}
	return nil, firstStatus, firstDetail
}

// streamAskedFor says what the caller's body asked for: a streamed answer, a whole answer at
// once, or nothing at all (no body, or a body that is not a JSON object with a top-level
// "stream"). Both the OpenAI shape and the Anthropic shape carry that flag in the same place.
//
// It exists for one question only: RunPod's proxy answers 524 after about 120 seconds of silence
// from the pod, and a request that did NOT ask for streaming is silent by definition until the
// whole answer is generated. The answer goes in the per-call log line, never on the page.
func streamAskedFor(body []byte) string {
	if len(body) == 0 {
		return "not asked for"
	}
	var asked struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &asked); err != nil || asked.Stream == nil {
		return "not asked for"
	}
	if *asked.Stream {
		return "on"
	}
	return "off"
}

// proxySeconds writes one of the per-call line's timings. A zero duration means the thing never
// happened (no headers, or no byte of body) and says so rather than printing "0.0s", which reads
// as instant.
func proxySeconds(d time.Duration) string {
	if d == 0 {
		return "never"
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// handleProjectProxy answers the fixed address, /p/<slug> and everything under it: the one place
// to point a client at, whatever that project is serving behind it right now. The slug is read
// off the path and refused by name (404) when it names no project this console knows. The
// request then names the model it wants in its body, and lands on the machine, among that
// project's, serving that model, so two machines can serve at once behind one project's address
// (proxyTarget). It forwards the request's method, path (with /p/<slug> stripped) and body to
// that machine's address, adding the machine's key, and streams the response back unbuffered,
// flushing after every chunk read from upstream, so a streamed chat completion arrives as it is
// generated. The machine's URL and key are never written to the page or to the log.
func (s *Server) handleProjectProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, projectProxyPrefix)
	project, upstreamPath, _ := strings.Cut(rest, "/")
	upstreamPath = "/" + upstreamPath

	if exists, err := s.store.ProjectExists(project); err != nil {
		s.log.Add("console", "could not check whether project "+project+" exists: "+err.Error())
		writeRefusal(w, http.StatusInternalServerError, "Could not check that project.")
		return
	} else if !exists {
		writeRefusal(w, http.StatusNotFound, "'"+project+"' is not a project this console knows.")
		return
	}

	// The body is read once, here, and every use of it comes off these bytes: which machine it
	// goes to, the rewrites, and what is forwarded. Above the cap nothing is forwarded, because
	// the whole body has to be held to read the model name out of it.
	limited := http.MaxBytesReader(w, r.Body, maxProxyBodyBytes)
	body, readErr := io.ReadAll(limited)
	if readErr != nil {
		var tooBig *http.MaxBytesError
		if errors.As(readErr, &tooBig) {
			writeRefusal(w, http.StatusRequestEntityTooLarge, "The request body is too large.")
			return
		}
		writeRefusal(w, http.StatusBadRequest, "Could not read the request body.")
		return
	}

	target, status, detail := s.proxyTarget(project, body)
	if target == nil {
		if status == http.StatusInternalServerError {
			s.log.Add("console", "could not check what project '"+project+"' is serving: "+detail)
		}
		// A refusal is a call through the fixed address too, and the per-call line below is only
		// written for a call that reached a machine, so every call leaves one line. It names the
		// method, the path and the status; never the body.
		s.log.Add("proxy", r.Method+" "+upstreamPath+" on '"+project+"' refused before any pod: status "+strconv.Itoa(status)+", "+detail)
		writeRefusal(w, status, detail)
		return
	}

	// Counted for as long as this handler runs, so the auto-move never destroys a machine in
	// the middle of an answer.
	inflightRef := machineRef{target.providerKey, target.providerID}
	s.requestStarted(inflightRef)
	defer s.requestEnded(inflightRef)

	// Only a POST to exactly /v1/messages, the path claude-code speaks, is inspected or rewritten.
	if r.Method == http.MethodPost && upstreamPath == "/v1/messages" {
		// The first such body to reach a given machine earns one log line naming the role order
		// it carries (roles only, never content), so what a client sends can be seen.
		roleOrderKey := target.providerKey + "/" + target.providerID
		s.roleOrderMu.Lock()
		alreadyLoggedRoles := s.roleOrderLogged[roleOrderKey]
		s.roleOrderLogged[roleOrderKey] = true
		s.roleOrderMu.Unlock()
		if !alreadyLoggedRoles {
			if desc, ok := describeRoleOrder(body); ok {
				s.log.Add("proxy", "first /v1/messages body on "+target.providerID+": "+desc)
			}
		}

		// Only a llama.cpp target's Qwen chat template refuses a system message that is not
		// part of one leading contiguous block, so only a llama.cpp target's body is normalised;
		// a vLLM target is never touched.
		if target.format == "gguf" {
			if normalized, changed := normalizeMessagesForLlamaCpp(body); changed {
				body = normalized
			}
		}
		// The body's thinking setting is forwarded as the caller sent it. Forcing thinking off
		// would not cure RunPod's proxy timeout and costs answer quality, so it is not done.
	}

	// A streamed /v1/chat/completions request that does not already ask for
	// stream_options.include_usage gets it added, so the usage scan below has a final chunk to
	// read; neither model server sends one otherwise. /v1/messages needs no such rewrite: vLLM
	// always turns that endpoint's stream_options.include_usage on internally, and llama.cpp's
	// Anthropic converter always puts the input and cache figures in message_start.
	if r.Method == http.MethodPost && upstreamPath == "/v1/chat/completions" {
		if injected, changed := injectStreamOptionsIncludeUsage(body); changed {
			body = injected
		}
	}

	upstreamURL := target.baseURL + upstreamPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeRefusal(w, http.StatusBadGateway, "Could not build the request to the serving machine.")
		return
	}
	// This program's key replaces whatever the caller sent, never both: the caller's access key
	// or session cookie (Authorization, X-Api-Key, Cookie) is never forwarded to a rented machine
	// or RunPod's proxy. The caller's User-Agent is not forwarded either (proxyClient sets the
	// one this program sends). Content-Length is dropped with it: the body forwarded is the bytes
	// read above, whose length http.NewRequest has already put on the outgoing request.
	copyProxyRequestHeaders(upReq.Header, r.Header)
	upReq.Header.Set("Authorization", "Bearer "+target.apiKey)
	// Never compressed, so the usage scan below never has to decode anything. Neither model
	// server is known to gzip a response here, but a caller's Accept-Encoding: gzip would
	// otherwise be forwarded.
	upReq.Header.Set("Accept-Encoding", "identity")

	// The per-call line records why a 524 can happen. RunPod's proxy gives up after about 120
	// seconds of silence from the pod, and the line shows whether the pod was silent because the
	// caller asked for a whole answer at once or for some other reason. Every call through the
	// fixed address writes one line: what was asked for, when the headers came back, when the
	// first byte of the answer came back, how much came back and how it ended. The machine is
	// named by its provider id; its address and its key are never written.
	streamAsk := streamAskedFor(body)
	upstreamStart := time.Now()
	var headersAfter, firstByteAfter time.Duration
	bodyBytes := 0
	outcome := "no answer"
	defer func() {
		s.log.Add("proxy", fmt.Sprintf(
			"%s %s on %s: streaming %s, headers %s, first byte %s, %d bytes, %s after %s",
			r.Method, upstreamPath, target.providerID, streamAsk,
			proxySeconds(headersAfter), proxySeconds(firstByteAfter), bodyBytes,
			outcome, proxySeconds(time.Since(upstreamStart))))
	}()

	resp, err := s.proxyClient.Do(upReq)
	if err != nil {
		// Never log err.Error() here: an http.Client error embeds the request URL, which carries
		// the machine's address (never shown, never logged).
		outcome = "the machine could not be reached"
		s.log.Add("proxy", "the serving machine could not be reached.")
		writeRefusal(w, http.StatusBadGateway, "The serving machine could not be reached.")
		return
	}
	defer resp.Body.Close()
	headersAfter = time.Since(upstreamStart)
	outcome = fmt.Sprintf("status %d", resp.StatusCode)
	// The response headers arriving already move the inactivity clock, on any status: an error
	// answer counts as output.
	s.recordOutput(machineRef{target.providerKey, target.providerID})

	// A copy of the answer is scanned for its usage alongside the unchanged write-and-flush
	// below, only on status 200: an error body carries no usage field in either model server's
	// shape. On any parse failure, or a call that carried no usage, nothing is counted for it.
	// There is no database write here, only the in-memory delta flushed once per live-poll pass
	// (recordUsage and flushTokenDeltas).
	var scanner *usageScanner
	if resp.StatusCode == http.StatusOK {
		scanner = newUsageScanner(streamAsk == "on", upstreamPath)
		defer func() {
			in, out, cached, ok := scanner.finish()
			if !ok {
				if scanner.failed {
					// The line names the machine and the path, never the body; the parse failure
					// carries no useful detail beyond this call's tokens going uncounted.
					s.log.Add("proxy", "could not read the token usage from "+target.providerID+" on "+upstreamPath+".")
				}
				return
			}
			s.recordUsage(machineRef{target.providerKey, target.providerID}, in, out, cached)
		}()
	}

	copyProxyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher, canFlush := w.(http.Flusher)

	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if firstByteAfter == 0 {
				firstByteAfter = time.Since(upstreamStart)
			}
			bodyBytes += n
			s.recordOutput(machineRef{target.providerKey, target.providerID})
			if scanner != nil {
				scanner.feed(buf[:n])
			}
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				outcome = fmt.Sprintf("status %d, the caller went away", resp.StatusCode)
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				if r.Context().Err() != nil {
					outcome = fmt.Sprintf("status %d, the caller went away", resp.StatusCode)
				} else {
					outcome = fmt.Sprintf("status %d, cut short", resp.StatusCode)
					s.log.Add("proxy", "the serving machine's own answer was cut short: "+proxyReadErrorText(readErr, target.baseURL)+".")
				}
			}
			return
		}
	}
}

// copyProxyRequestHeaders copies the caller's headers onto the request going to the serving
// machine, except the ones that are this program's to set or that are the caller's credentials to
// this program (access key, session cookie), which a rented machine or RunPod's proxy never see.
func copyProxyRequestHeaders(dst, src http.Header) {
	for name, values := range src {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "X-Api-Key") || strings.EqualFold(name, "Cookie") ||
			strings.EqualFold(name, "Host") || strings.EqualFold(name, "Content-Length") || strings.EqualFold(name, "User-Agent") {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}

// copyProxyResponseHeaders copies the serving machine's answer headers to the caller. A cookie
// from the machine or its proxy never reaches the caller's browser, and neither does a policy
// header it chose: the answer is served from the console's origin, so it gets the sandbox policy
// instead and no cross-origin permission.
func copyProxyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		if strings.EqualFold(name, "Set-Cookie") || strings.EqualFold(name, "Content-Security-Policy") ||
			strings.EqualFold(name, "Content-Security-Policy-Report-Only") || strings.HasPrefix(strings.ToLower(name), "access-control-") {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
	dst.Set("Content-Security-Policy", "sandbox")
}

// proxyReadErrorText says what ended a read of the serving machine's answer, without the
// machine's address (never logged). A network error is reduced to its operation and its cause,
// which carry no address; anything else has the machine's host removed from its text.
func proxyReadErrorText(err error, baseURL string) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Op + ": " + opErr.Err.Error()
	}
	text := err.Error()
	if u, perr := url.Parse(baseURL); perr == nil && u.Host != "" {
		text = strings.ReplaceAll(text, u.Host, "the machine")
	}
	return text
}
