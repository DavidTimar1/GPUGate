// Package destroyer is the one destroy: keep asking a provider to destroy a machine until it
// accepts, then wait for a fresh list that no longer carries the machine, then record it gone.
// The console (Delete, the idle sweep, the mover, a resumed destroy) and the `gpugate destroy`
// command both call Run, so a machine is destroyed the same way whoever asks. It knows nothing
// of the server: what the console does on the way (strip notices, closing a tunnel) comes in
// through Hooks.
package destroyer

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/DavidTimar1/gpugate/internal/podkey"
	"github.com/DavidTimar1/gpugate/internal/provider"
	"github.com/DavidTimar1/gpugate/internal/store"
)

// RetryInterval is how often a refused destroy is retried and how often a provider is re-asked
// to confirm a machine gone.
const RetryInterval = 15 * time.Second

// GiveUpAfter bounds how long Run keeps retrying before it stops and says the machine is still
// billing. It is the one clock allowed past the hour every other clock in the launch path is held
// to, because a destroy is what stops money going out. The figure only keeps a destroy of an id
// that never existed from retrying forever.
const GiveUpAfter = 4 * time.Hour

// escalateAfter is how long a destroy may fail before the log names the by-hand command, and
// how often that line repeats.
const escalateAfter = 5 * time.Minute

const callTimeout = 30 * time.Second

// MachineGetter is implemented by a provider client that can answer one machine directly by id.
type MachineGetter interface {
	GetMachine(ctx context.Context, providerID string) (provider.RunningMachine, error)
}

// Hooks carries what the caller does on the way. Log is required. Notice and Gone may be nil.
type Hooks struct {
	// Log writes one line under a log source ("instances", "providers", "console").
	Log func(category, text string)
	// Notice sets the short "still billing" line the console shows on the machine's row.
	Notice func(text string)
	// Gone runs once a fresh list has confirmed the machine gone and the record is written.
	Gone func()
}

// HandCommand is the command that destroys one machine by hand, with the ids filled in.
func HandCommand(providerKey, providerID string) string {
	return "./dist/gpugate destroy " + providerKey + " " + providerID
}

// Run destroys providerID on target and returns when a fresh list has confirmed it gone, or
// after GiveUpAfter has passed from askedAt (the destroy_intent row's asked_at, so a resumed
// destroy does not get a fresh ceiling). The intent row must already be written. It reports
// whether the machine was confirmed gone.
func Run(target provider.Provider, st *store.Store, providerID, displayName string, askedAt time.Time, h Hooks) bool {
	notice := func(text string) {
		if h.Notice != nil {
			h.Notice(text)
		}
	}
	deadline := askedAt.Add(GiveUpAfter)
	// The escalation clock runs across both loops below: the machine bills in both, so a destroy
	// refused for four minutes and then unconfirmed for two more has been billing for six.
	nextEscalation := askedAt.Add(escalateAfter)
	escalate := func() {
		if time.Now().Before(nextEscalation) {
			return
		}
		cmd := HandCommand(target.Key(), providerID)
		h.Log("instances", displayName+" ("+providerID+") on "+target.DisplayName()+
			" is still billing after "+escalateAfter.String()+" of retrying its destroy. By hand: "+cmd)
		notice("still billing for over " + escalateAfter.String() + ", by hand: " + cmd)
		nextEscalation = time.Now().Add(escalateAfter)
	}
	giveUp := func(reason string) {
		h.Log("instances", displayName+" ("+providerID+") on "+target.DisplayName()+" "+reason+
			", after retrying for "+GiveUpAfter.String()+". IT IS STILL BILLING.")
		notice("still billing: " + reason)
	}
	ctx := context.Background()

	for {
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		err := target.DestroyMachine(callCtx, providerID)
		cancel()
		if err == nil {
			break
		}
		if errors.Is(err, provider.ErrMachineNotFound) {
			// The provider already has nothing by this id: its answer that it is gone. The confirm
			// loop below still runs a fresh list before marking it gone.
			h.Log("instances", target.DisplayName()+" says "+displayName+" ("+providerID+") is already gone.")
			break
		}
		h.Log("instances", target.DisplayName()+" refused to destroy "+displayName+" ("+providerID+"): "+err.Error())
		// Verda's DestroyMachine turns every error status into a refusal and never reports
		// ErrMachineNotFound, so a refusal is followed by one direct read: not found there is the
		// provider's answer that it is gone, and the confirm loop below still decides.
		if getter, ok := target.(MachineGetter); ok {
			readCtx, readCancel := context.WithTimeout(ctx, callTimeout)
			read, readErr := getter.GetMachine(readCtx, providerID)
			readCancel()
			// Only these two words mean gone: an exited RunPod pod still exists and bills storage.
			readState := strings.ToUpper(strings.TrimSpace(read.State))
			if errors.Is(readErr, provider.ErrMachineNotFound) || (readErr == nil && (readState == "DISCONTINUED" || readState == "NOTFOUND")) {
				h.Log("instances", target.DisplayName()+" reports "+displayName+" ("+providerID+") is gone, so its refused destroy is not retried.")
				break
			}
		}
		if time.Now().After(deadline) {
			giveUp("refused to destroy it: " + err.Error())
			return false
		}
		notice("still billing, " + target.DisplayName() + " is refusing to destroy it: " + err.Error())
		escalate()
		time.Sleep(RetryInterval)
	}

	for {
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		machines, err := target.ListMachines(callCtx)
		cancel()
		if err != nil {
			h.Log("providers", target.DisplayName()+" could not be asked to confirm the destroy of "+displayName+": "+err.Error())
		} else {
			stillThere := false
			for _, m := range machines {
				if m.ProviderID == providerID {
					stillThere = true
					break
				}
			}
			if !stillThere {
				if werr := st.MarkDestroyGone(target.Key(), providerID); werr != nil {
					h.Log("console", "could not record that "+displayName+" is gone: "+werr.Error())
				}
				// The key file goes only once a fresh answer has confirmed the machine gone, never
				// on our own destroy call succeeding.
				if werr := podkey.Delete(target.Key(), providerID); werr != nil {
					h.Log("console", "could not remove the pod key for "+providerID+": "+werr.Error())
				}
				h.Log("instances", displayName+" ("+providerID+") on "+target.DisplayName()+" is confirmed gone.")
				if h.Gone != nil {
					h.Gone()
				}
				return true
			}
		}
		if time.Now().After(deadline) {
			giveUp("was asked to destroy it, but no fresh answer has confirmed it gone")
			return false
		}
		notice("still billing, waiting for " + target.DisplayName() + " to confirm it is gone")
		escalate()
		time.Sleep(RetryInterval)
	}
}
