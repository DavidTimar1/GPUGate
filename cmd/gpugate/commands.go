package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/DavidTimar1/gpugate/internal/destroyer"
	"github.com/DavidTimar1/gpugate/internal/login"
	"github.com/DavidTimar1/gpugate/internal/provider"
	"github.com/DavidTimar1/gpugate/internal/store"
)

const usage = `usage:
  gpugate                         run the console
  gpugate list                    print every machine on both accounts
  gpugate destroy <provider> <id> destroy one machine and wait for a fresh list to confirm it gone
  gpugate set-password            set or replace the console password (asks twice, shows nothing)
  gpugate make-key                print one random access key for GPUGATE_ACCESS_KEY, and nothing else`

// commandCallTimeout bounds one provider call made by a command.
const commandCallTimeout = 30 * time.Second

// runCommand runs a subcommand. A command never starts the server's loops: it asks the providers
// and, for destroy, uses the same destroyer the console uses. Commands read the database beside
// the working directory, so they refuse to run from anywhere that has none rather than create an
// empty one.
func runCommand(args []string) error {
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New(usage)
		}
		return commandList()
	case "set-password":
		if len(args) != 1 {
			return errors.New(usage)
		}
		return commandSetPassword()
	case "destroy":
		if len(args) != 3 {
			return errors.New(usage)
		}
		return commandDestroy(args[1], args[2])
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

// requireProjectFolder refuses unless the database file is where the program expects it.
func requireProjectFolder() error {
	if _, err := os.Stat(dbPath); err != nil {
		wd, _ := os.Getwd()
		return fmt.Errorf("there is no %s under %s; run this command from the project folder (the one gate.sh lives in)", dbPath, filepath.Base(wd))
	}
	return nil
}

// commandList prints every machine each provider says the account holds. A provider that cannot
// be asked is said so: it is never printed as "none".
func commandList() error {
	if err := requireProjectFolder(); err != nil {
		return err
	}
	for _, p := range buildProviders(func(string) {}) {
		if !p.CredentialsConfigured() {
			fmt.Printf("%s: could not be asked: no credential configured\n", p.DisplayName())
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), commandCallTimeout)
		machines, err := p.ListMachines(ctx)
		cancel()
		if err != nil {
			fmt.Printf("%s: could not be asked: %s\n", p.DisplayName(), err.Error())
			continue
		}
		if len(machines) == 0 {
			fmt.Printf("%s: no machines (a fresh answer)\n", p.DisplayName())
			continue
		}
		for _, m := range machines {
			fmt.Printf("%s  id=%s  name=%s  state=%s  %s\n", p.Key(), m.ProviderID, m.DisplayName, m.State, provider.PriceText(m.PricePerHour))
		}
	}
	return nil
}

// commandDestroy destroys one machine: the intent is written first, then destroyer.Run
// asks until the provider accepts and a fresh list no longer carries it. Each step is printed.
func commandDestroy(providerKey, providerID string) error {
	if err := requireProjectFolder(); err != nil {
		return err
	}
	target := provider.ByKey(buildProviders(func(string) {}), providerKey)
	if target == nil {
		return fmt.Errorf("%q is not a provider this program knows (verda or runpod)", providerKey)
	}
	if !target.CredentialsConfigured() {
		return fmt.Errorf("%s has no credential configured, so it cannot be asked to destroy anything", target.DisplayName())
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer st.Close()

	// Name and price for the intent row, best effort from one fresh list; the destroy itself
	// needs only the id.
	displayName := providerKey + " machine " + providerID
	var price *float64
	ctx, cancel := context.WithTimeout(context.Background(), commandCallTimeout)
	machines, listErr := target.ListMachines(ctx)
	cancel()
	switch {
	case listErr != nil:
		fmt.Printf("could not read %s's list before destroying (%s); going on with the id alone\n", target.DisplayName(), listErr.Error())
	default:
		found := false
		for _, m := range machines {
			if m.ProviderID == providerID {
				displayName, price, found = m.DisplayName, m.PricePerHour, true
				break
			}
		}
		if !found {
			fmt.Printf("%s does not list %s now; asking it to destroy it anyway\n", target.DisplayName(), providerID)
		}
	}

	say := func(text string) { fmt.Println(time.Now().Format("15:04:05") + " " + text) }
	say("recording the destroy of " + displayName + " (" + providerID + ")")
	if err := st.RecordDestroyIntent(providerKey, providerID, displayName, price); err != nil {
		return fmt.Errorf("could not record the destroy; nothing was asked of %s: %w", target.DisplayName(), err)
	}
	say("recorded; asking " + target.DisplayName() + " to destroy it")
	lastNotice := ""
	gone := destroyer.Run(target, st, providerID, displayName, time.Now(), destroyer.Hooks{
		Log: func(_, text string) { say(text) },
		Notice: func(text string) {
			if text != lastNotice {
				lastNotice = text
				say(text)
			}
		},
	})
	if !gone {
		return fmt.Errorf("%s is NOT confirmed gone and may still be billing", displayName)
	}
	say("done: " + displayName + " is gone and its record is closed")
	return nil
}

// commandSetPassword creates .state when it is missing, so it needs no database and can run
// before the program ever has. It asks for the console password twice and writes its hash. At a terminal
// nothing typed is shown; with a pipe on standard input it reads two lines instead. The password
// is never printed or logged. A running server notices the new file by itself and every session
// made under the old password ends.
func commandSetPassword() error {
	var first, second string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		var err error
		if first, err = promptHidden("New password: "); err != nil {
			return err
		}
		if second, err = promptHidden("New password again: "); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(os.Stderr, "warning: standard input is not a terminal, so the password is being read from a pipe and may sit in a script or a shell history")
		in := bufio.NewReader(os.Stdin)
		var err error
		if first, err = readLine(in); err != nil {
			return fmt.Errorf("read the password from standard input: %w", err)
		}
		if second, err = readLine(in); err != nil {
			return fmt.Errorf("read the password again from standard input: %w", err)
		}
	}
	if first == "" {
		return errors.New("the password is empty; nothing was changed")
	}
	if first != second {
		return errors.New("the two entries differ; nothing was changed")
	}
	if err := login.SetPassword(login.HashPath, first); err != nil {
		return err
	}
	fmt.Println("password set; every browser that was logged in is logged out")
	return nil
}

func promptHidden(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read the password: %w", err)
	}
	return string(raw), nil
}

// readLine reads one line without its line ending and without trimming anything else.
func readLine(in *bufio.Reader) (string, error) {
	line, err := in.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
