package console

import (
	"github.com/DavidTimar1/gpugate/internal/provider"
	"github.com/DavidTimar1/gpugate/internal/sshtunnel"
)

// ensureTunnel answers ref's SSH tunnel to sshAddr, creating one when none exists, or rebuilding
// it when the address a fresh answer just named differs from the one it was built for. A nil
// s.sshKey (no usable key file) answers nil and logs once per ref, never once per pass.
func (s *Server) ensureTunnel(ref machineRef, sshAddr string) *sshtunnel.Tunnel {
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()

	if existing, have := s.tunnels[ref]; have {
		if existing.Addr() == sshAddr {
			return existing
		}
		existing.Close()
		delete(s.tunnels, ref)
		s.log.Add("launch", providerLabel(ref.providerKey)+" "+ref.providerID+": the box's address changed, rebuilding its tunnel.")
	}
	if s.sshKey == nil {
		if !s.noKeyLogged[ref] {
			s.noKeyLogged[ref] = true
			s.log.Add("launch", "no SSH key could be loaded from "+sshtunnel.PrivateKeyPath+"; a tunnel to "+ref.providerID+" cannot be opened.")
		}
		return nil
	}
	pin, err := s.store.SSHHostKey(ref.providerKey, ref.providerID)
	if err != nil {
		s.log.Add("console", "could not read the pinned SSH host key for "+ref.providerID+": "+err.Error())
	}
	logPrefix := providerLabel(ref.providerKey) + " " + ref.providerID + ": "
	t, err := sshtunnel.New(sshAddr, s.sshKey, pin,
		func(line string) {
			if werr := s.store.SetSSHHostKey(ref.providerKey, ref.providerID, line); werr != nil {
				s.log.Add("console", "could not pin the SSH host key for "+ref.providerID+": "+werr.Error())
			}
		},
		func(text string) { s.log.Add("launch", logPrefix+text) },
	)
	if err != nil {
		s.log.Add("launch", logPrefix+"could not open its SSH tunnel: "+err.Error())
		return nil
	}
	s.tunnels[ref] = t
	return t
}

// ensureTunnels opens or rebuilds a tunnel for every fresh machine on providerKey with a
// non-empty SSHAddr whose claim is loading or serving right now. It is called once per live-poll
// pass, after sweepInactivity, so a restart rebuilds every serving Verda claim's tunnel from the
// first fresh answer.
func (s *Server) ensureTunnels(providerKey string, machines []provider.RunningMachine) {
	// A tunnel whose machine this fresh list no longer carries is closed: a machine destroyed
	// from the command line (or by anything outside this process) must not leave one open.
	listed := make(map[string]bool, len(machines))
	for _, m := range machines {
		listed[m.ProviderID] = true
	}
	// Two consecutive successful lists must lack the machine: one stale list must not cut a
	// working machine's tunnel.
	var vanished []machineRef
	s.tunnelMu.Lock()
	for ref := range s.tunnels {
		if ref.providerKey != providerKey {
			continue
		}
		if listed[ref.providerID] {
			delete(s.tunnelMisses, ref)
			continue
		}
		s.tunnelMisses[ref]++
		if s.tunnelMisses[ref] >= 2 {
			vanished = append(vanished, ref)
		}
	}
	s.tunnelMu.Unlock()
	for _, ref := range vanished {
		s.closeTunnel(ref)
		s.log.Add("launch", providerLabel(ref.providerKey)+" "+ref.providerID+": no longer listed, so its SSH tunnel was closed.")
	}
	info, err := s.store.ServingInfoByProviderID(providerKey)
	if err != nil {
		s.log.Add("console", "could not read "+providerLabel(providerKey)+"'s serving info for its tunnels: "+err.Error())
		return
	}
	for _, m := range machines {
		if m.SSHAddr == "" {
			continue
		}
		si, have := info[m.ProviderID]
		if !have || (si.ServingState != "loading" && si.ServingState != "serving") {
			continue
		}
		s.ensureTunnel(machineRef{providerKey, m.ProviderID}, m.SSHAddr)
	}
}

// closeTunnel closes and forgets ref's tunnel, a no-op when it has none. Called when a destroy is
// confirmed gone, from giveUpOnClaim, and from ensureTunnels when a fresh list no longer carries
// the machine.
func (s *Server) closeTunnel(ref machineRef) {
	s.tunnelMu.Lock()
	t, have := s.tunnels[ref]
	if have {
		delete(s.tunnels, ref)
	}
	delete(s.noKeyLogged, ref)
	delete(s.tunnelMisses, ref)
	s.tunnelMu.Unlock()
	if have {
		t.Close()
	}
}

// servingBaseURL answers where m's model server is reached from this process right now:
// m.ServingBaseURL unchanged for RunPod (m.SSHAddr == ""), or a Verda machine's tunnel local URL
// once one exists, with stage naming why it is not usable yet ("" once the tunnel is
// connected). Every reader of a machine's serving address reads this rather than
// RunningMachine.ServingBaseURL directly.
func (s *Server) servingBaseURL(m provider.RunningMachine) (url, stage string) {
	if m.SSHAddr == "" {
		return m.ServingBaseURL, ""
	}
	t := s.ensureTunnel(machineRef{m.ProviderKey, m.ProviderID}, m.SSHAddr)
	if t == nil {
		return "", "no SSH key on this machine"
	}
	if !t.Connected() {
		return t.LocalBaseURL(), "waiting for SSH on the box to accept (" + t.LastAttempt() + ")"
	}
	return t.LocalBaseURL(), ""
}

// canReadLog reports whether ref's container log can be read at all right now: a tunnel on file
// for it (Verda, connected or not; readRedactedPodLog answers plainly when it is down) or the
// provider's log getter (RunPod).
func (s *Server) canReadLog(ref machineRef) bool {
	s.tunnelMu.Lock()
	_, haveTunnel := s.tunnels[ref]
	s.tunnelMu.Unlock()
	if haveTunnel {
		return true
	}
	return s.logGetter(ref.providerKey) != nil
}
