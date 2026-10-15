package console

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/DavidTimar1/gpugate/internal/podkey"
	"github.com/DavidTimar1/gpugate/internal/provider"
)

// providerMachineGetter is implemented by a provider client that can answer one machine directly
// by id, bypassing the account-wide list; both RunPod's and Verda's clients implement
// GetMachine. It is not part of provider.Provider: it is an escape hatch for the readiness
// poll's tie-breaker, not a fact every provider must carry.
type providerMachineGetter interface {
	GetMachine(ctx context.Context, providerID string) (provider.RunningMachine, error)
}

// machineGetter answers the registered providerKey provider's providerMachineGetter, or nil when
// none is registered or it implements no such method. podGoneByDirectRead and
// logReadinessStatus look it up here.
func (s *Server) machineGetter(providerKey string) providerMachineGetter {
	for _, p := range s.providers {
		if p.Key() != providerKey {
			continue
		}
		if g, ok := p.(providerMachineGetter); ok {
			return g
		}
		break
	}
	return nil
}

// podGoneByDirectRead asks providerKey directly, once, whether providerID still exists. It is the
// tie-breaker pollServingReadiness reaches for only after servingVanishMissesNeeded consecutive
// fresh live-poll answers have missed a machine. provider.ErrMachineNotFound is gone; a
// successful read whose state word is one of exitedStateWords is gone too: a destroyed Verda
// instance answers 200 with `status` `discontinued`, never a 404 (docs/providers.md#verda). Any
// other answer or error means the provider could not be asked, or answered something else, so it
// is read as "still there, keep waiting" and logged plainly, never as gone.
func (s *Server) podGoneByDirectRead(providerKey, providerID string) bool {
	getter := s.machineGetter(providerKey)
	if getter == nil {
		s.log.Add("launch", providerLabel(providerKey)+" "+providerID+": no direct read is available, so it could not be confirmed gone.")
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerCallTimeout)
	defer cancel()
	m, err := getter.GetMachine(ctx, providerID)
	if err == nil {
		return exitedStateWords[strings.ToUpper(strings.TrimSpace(m.State))]
	}
	if errors.Is(err, provider.ErrMachineNotFound) {
		return true
	}
	s.log.Add("launch", providerLabel(providerKey)+" "+providerID+" could not be confirmed gone by a direct read: "+err.Error())
	return false
}

// providerLogGetter is implemented by a provider client that can read a machine's container log,
// bounded: RunPod's client (runpod.Client.GetLogs) only. Verda has no log endpoint, so its log
// is read over the SSH tunnel instead (readRedactedPodLog). It is not part of provider.Provider,
// for the reason given at providerMachineGetter.
type providerLogGetter interface {
	GetLogs(ctx context.Context, providerID string, tail int) ([]string, error)
}

// logGetter answers the registered providerKey provider's providerLogGetter, or nil when none is
// registered or it implements no such method.
func (s *Server) logGetter(providerKey string) providerLogGetter {
	for _, p := range s.providers {
		if p.Key() != providerKey {
			continue
		}
		if g, ok := p.(providerLogGetter); ok {
			return g
		}
		break
	}
	return nil
}

// redactAndCapLogLines replaces every occurrence of key in each line with "[key]" (key itself is
// never logged) and caps each line at 300 characters: the machine's log is untrusted text from a
// machine on the internet, read as data, never trusted at face value or size.
func redactAndCapLogLines(lines []string, key string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if key != "" {
			l = strings.ReplaceAll(l, key, "[key]")
		}
		if len(l) > 300 {
			l = l[:300]
		}
		out = append(out, l)
	}
	return out
}

// readRedactedPodLog reads providerID's container log, up to tail lines, then replaces every
// occurrence of its key (read from the key file only, podkey.Read) with "[key]" and caps each
// line at 300 characters. It tries the tunnel registry first: a tunnel on file for this machine
// runs `tail -n <tail> /var/log/gpugate-serve.log` over it when connected, or answers "the SSH
// tunnel to the box is down" when not, without falling through to a provider log getter. No
// tunnel falls through to providerKey's log getter (RunPod only) or, with neither, "no log
// reader is available". Any read failure is returned as an error rather than guessed at; the
// caller decides what that means for whatever it was about to do.
func (s *Server) readRedactedPodLog(providerKey, providerID string, tail int) ([]string, error) {
	ref := machineRef{providerKey, providerID}
	s.tunnelMu.Lock()
	t, haveTunnel := s.tunnels[ref]
	s.tunnelMu.Unlock()

	key, _ := podkey.Read(providerKey, providerID) // "" when none is on file; nothing to redact then

	if haveTunnel {
		if !t.Connected() {
			return nil, fmt.Errorf("the SSH tunnel to the box is down")
		}
		ctx, cancel := context.WithTimeout(context.Background(), providerCallTimeout)
		defer cancel()
		raw, err := t.Exec(ctx, "tail -n "+strconv.Itoa(tail)+" /var/log/gpugate-serve.log")
		if err != nil {
			return nil, err
		}
		var lines []string
		if trimmed := strings.TrimRight(string(raw), "\n"); trimmed != "" {
			lines = strings.Split(trimmed, "\n")
		}
		return redactAndCapLogLines(lines, key), nil
	}

	getter := s.logGetter(providerKey)
	if getter == nil {
		return nil, fmt.Errorf("no log reader is available for %s", providerLabel(providerKey))
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerCallTimeout)
	defer cancel()
	lines, err := getter.GetLogs(ctx, providerID, tail)
	if err != nil {
		return nil, err
	}
	return redactAndCapLogLines(lines, key), nil
}

// startupScripter is implemented by a provider client that can create and delete a startup
// script: Verda's client only (verda.Client.CreateStartupScript and DeleteStartupScript). RunPod
// has no startup script. It is not part of provider.Provider, for the reason given at
// providerMachineGetter.
type startupScripter interface {
	CreateStartupScript(ctx context.Context, o provider.CreateOptions) (scriptID string, err error)
	DeleteStartupScript(ctx context.Context, scriptID string) error
}

// sshKeyLister is implemented by a provider client whose machines are reached over this
// program's SSH tunnel: Verda's client only (verda.Client.ListSSHPublicKeys). It is not part of
// provider.Provider, for the reason given at providerMachineGetter. startLaunch's key-match check
// reads it before any claim is written.
type sshKeyLister interface {
	ListSSHPublicKeys(ctx context.Context) ([]string, error)
}

// startupScripterFor answers the registered providerKey provider's startupScripter, or nil when
// none is registered or it implements no such method.
func (s *Server) startupScripterFor(providerKey string) startupScripter {
	for _, p := range s.providers {
		if p.Key() != providerKey {
			continue
		}
		if g, ok := p.(startupScripter); ok {
			return g
		}
		break
	}
	return nil
}

// deleteStartupScriptForClaim deletes claimID's Verda startup script, reading its id back from
// the store rather than threading it through every caller's signature. A no-op when providerKey
// has no startupScripter (RunPod, always) or the claim carries no script id. A failed delete is
// logged and left: the next resumed poll ends the same way and tries again.
func (s *Server) deleteStartupScriptForClaim(providerKey string, claimID int64) {
	scripter := s.startupScripterFor(providerKey)
	if scripter == nil {
		return
	}
	id, err := s.store.ClaimStartupScriptID(claimID)
	if err != nil {
		s.log.Add("launch", "could not read the startup script id for claim "+strconv.FormatInt(claimID, 10)+": "+err.Error())
		return
	}
	if id == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerCallTimeout)
	defer cancel()
	if err := scripter.DeleteStartupScript(ctx, *id); err != nil {
		s.log.Add("launch", providerLabel(providerKey)+" could not delete the startup script "+*id+" for claim "+strconv.FormatInt(claimID, 10)+": "+err.Error())
		return
	}
	s.log.Add("launch", providerLabel(providerKey)+" deleted the startup script "+*id+" for claim "+strconv.FormatInt(claimID, 10)+".")
}
