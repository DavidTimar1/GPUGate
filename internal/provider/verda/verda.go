// Package verda is the Verda GPU cloud behind the provider.Provider interface.
//
// Verda's control-plane API is OAuth2 client-credentials over REST. [DOC]
// https://api.verda.com/v1/openapi.json:
//   - POST /v1/oauth2/token answers a bearer token from {grant_type, client_id, client_secret}.
//   - GET /v1/instance-types answers the shape catalog: instance_type (the shape code), gpu
//     (number_of_gpus), gpu_memory (size_in_gigabytes, EVERY CARD SUMMED),
//     model (the card name), price_per_hour (a STRING).
//   - GET /v1/instance-availability answers one row per location that has anything free:
//     {location_code, availabilities: [shape codes]}. Spot is never asked about.
//
// A row with a model chosen (safetensors or fp8; ServesFormat refuses gguf) is created already
// serving it: a startup script hands a `docker run` of the official `vllm/vllm-openai:latest`
// image to a transient systemd unit, so the ~9 GiB pull never sits under the startup script's own
// timeout (see buildStartupScript).
package verda

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DavidTimar1/gpugate/internal/provider"
)

const (
	baseURL      = "https://api.verda.com/v1"
	callTimeout  = 30 * time.Second
	providerKey  = "verda"
	providerName = "Verda"
)

// Client is Verda behind the provider.Provider interface. It holds the two credentials in memory
// and never writes them down: they are passed in at construction and never printed, logged or
// returned to a caller.
type Client struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client

	tokenMu    sync.Mutex
	token      string
	tokenUntil time.Time
}

// New builds a Verda client from the two credential values. It makes no network call: the token
// is fetched lazily on first use, so constructing this object costs nothing. logf is where a slow
// or failing outbound call is reported (provider.LogFunc); the client's *http.Transport is the
// one every provider client shares (provider.NewHTTPClient).
func New(clientID, clientSecret string, logf provider.LogFunc) *Client {
	return &Client{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   provider.NewHTTPClient(providerName, callTimeout, logf),
	}
}

func (c *Client) Key() string         { return providerKey }
func (c *Client) DisplayName() string { return providerName }

// CredentialsConfigured reports whether both VERDA_MAIN_CLIENT_ID and VERDA_MAIN_CLIENT_SECRET
// were non-empty when this client was built.
func (c *Client) CredentialsConfigured() bool {
	return c.clientID != "" && c.clientSecret != ""
}

// ServesFormat answers whether Verda can serve a model published in format. Verda serves through
// vLLM's official image only: safetensors and fp8 answer true; gguf is refused by name, because
// that image carries no GGUF plugin and this program never rebuilds an image to add one. GGUF
// stays on RunPod's llama.cpp path.
func (c *Client) ServesFormat(format string) (bool, string) {
	switch format {
	case "safetensors", "fp8":
		return true, ""
	default:
		return false, "Verda serves through vLLM only; GGUF is llama.cpp, on RunPod"
	}
}

// ServesShape answers whether Verda's serving path can run on the shape with this code. A code
// ending ".CC" is a confidential-computing shape: Verda offers it only confidentialComputing
// images and no Docker image, and serving here runs a Docker container, so chooseHostImage would
// find nothing at launch (docs/providers.md#verda). This judges the same fact early, so the table
// shows the row as not launchable instead of the launch panel refusing.
func (c *Client) ServesShape(shapeCode string) (bool, string) {
	if strings.HasSuffix(shapeCode, ".CC") {
		return false, "Confidential-computing shape: Verda offers no Docker image for it, and serving runs in Docker. Pick the shape without .CC."
	}
	return true, ""
}

// instanceType is one row of GET /v1/instance-types, holding only the fields this program reads.
// price_per_hour arrives as a string on Verda's schema.
type instanceType struct {
	InstanceType string `json:"instance_type"`
	DisplayName  string `json:"display_name"`
	Model        string `json:"model"`
	Gpu          struct {
		NumberOfGpus int `json:"number_of_gpus"`
	} `json:"gpu"`
	GpuMemory struct {
		SizeInGigabytes int `json:"size_in_gigabytes"`
	} `json:"gpu_memory"`
	PricePerHour string `json:"price_per_hour"`
}

type availabilityRow struct {
	LocationCode   string   `json:"location_code"`
	Availabilities []string `json:"availabilities"`
}

// ListShapes asks Verda for its shape catalog and its live on-demand availability, and merges
// the two into one list. Costs nothing. An error here means Verda could not be asked at all,
// and the caller must report that rather than showing an empty table.
func (c *Client) ListShapes(ctx context.Context) ([]provider.Shape, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("verda: %w", err)
	}

	var types []instanceType
	if err := c.getJSON(ctx, token, "/instance-types", nil, &types); err != nil {
		return nil, fmt.Errorf("verda: list instance types: %w", err)
	}

	var availability []availabilityRow
	if err := c.getJSON(ctx, token, "/instance-availability", nil, &availability); err != nil {
		return nil, fmt.Errorf("verda: list availability: %w", err)
	}
	locationsByShape := map[string][]string{}
	for _, row := range availability {
		for _, code := range row.Availabilities {
			locationsByShape[code] = append(locationsByShape[code], row.LocationCode)
		}
	}

	shapes := make([]provider.Shape, 0, len(types))
	for _, t := range types {
		gpuCount := t.Gpu.NumberOfGpus
		wholeVRAM := t.GpuMemory.SizeInGigabytes
		// A shape with no card memory at all is left out: such a machine is not one we use.
		if wholeVRAM <= 0 {
			continue
		}
		var price *float64
		if t.PricePerHour != "" {
			if v, err := strconv.ParseFloat(t.PricePerHour, 64); err == nil {
				price = &v
			}
		}
		facts, isMIG, matched := provider.LookupCardFacts(t.Model)
		shape := provider.Shape{
			ProviderKey:          providerKey,
			Code:                 t.InstanceType,
			DisplayName:          firstNonEmpty(t.DisplayName, t.InstanceType),
			GPUName:              t.Model,
			GPUCount:             gpuCount,
			WholeMachineVRAMGB:   wholeVRAM,
			OnDemandPricePerHour: price,
			OrderableLocations:   locationsByShape[t.InstanceType],
		}
		if matched {
			shape.ComputeCapability = facts.ComputeCapability
			arch := facts.Architecture
			shape.CardArchitecture = &arch
			shape.CardYear = facts.Year
			if !isMIG {
				shape.CardFP8 = facts.FP8()
			}
		}
		shapes = append(shapes, shape)
	}
	return shapes, nil
}

// runningInstance is one row of GET /v1/instances, holding only the fields this program reads.
// [DOC] https://api.verda.com/v1/openapi.json, GetInstanceResponsePublicApiDto: unlike
// instance-types, price_per_hour here is a NUMBER, not a string. IP is required on that schema
// and nullable: "Public address; null when the instance is not reachable from the internet".
type runningInstance struct {
	ID           string  `json:"id"`
	InstanceType string  `json:"instance_type"`
	Hostname     string  `json:"hostname"`
	Description  string  `json:"description"`
	Location     string  `json:"location"`
	PricePerHour float64 `json:"price_per_hour"`
	Status       string  `json:"status"`
	IP           *string `json:"ip"`
}

// rowToRunningMachine turns one GET /v1/instances or GET /v1/instances/{id} row into the shape
// this program tracks a machine by. ListMachines, CreateMachine's read-back and GetMachine all
// use it, so all three answer the identical shape for the identical row. SSHAddr is set only once
// row.IP is non-nil and non-empty; while Verda has not named an address it is "", which the
// readiness poll (pollServingReadiness) reads as "not yet known", never as a refusal.
// ServingBaseURL is never set on Verda: nothing answers on the machine's public port 8000
// (docs/providers.md#verda: only port 22 answers), so a URL naming it would be a lie.
func rowToRunningMachine(row runningInstance) provider.RunningMachine {
	price := row.PricePerHour
	m := provider.RunningMachine{
		ProviderKey:  providerKey,
		ProviderID:   row.ID,
		ShapeCode:    row.InstanceType,
		DisplayName:  firstNonEmpty(row.Description, row.InstanceType),
		GPUName:      row.InstanceType,
		Hostname:     row.Hostname,
		Location:     row.Location,
		PricePerHour: &price,
		State:        row.Status,
	}
	if row.IP != nil && *row.IP != "" {
		m.SSHAddr = *row.IP + ":22"
	}
	return m
}

// ListMachines asks Verda for every machine this account holds right now. Costs nothing. An
// error means Verda could not be asked at all, and the caller must report that rather than
// showing nothing running.
func (c *Client) ListMachines(ctx context.Context) ([]provider.RunningMachine, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("verda: %w", err)
	}
	var rows []runningInstance
	if err := c.getJSON(ctx, token, "/instances", nil, &rows); err != nil {
		return nil, fmt.Errorf("verda: list instances: %w", err)
	}
	machines := make([]provider.RunningMachine, 0, len(rows))
	for _, row := range rows {
		machines = append(machines, rowToRunningMachine(row))
	}
	return machines, nil
}

// GetMachine asks Verda directly for one instance by id, bypassing the account-wide list. Costs
// nothing. Used only as the readiness poll's tie-breaker before it gives up waiting on a machine
// that consecutive fresh live-poll answers have missed (console.podGoneByDirectRead). A deleted
// instance answers 200 with status "discontinued" (docs/providers.md#verda); a 404 is mapped to
// provider.ErrMachineNotFound as well, and podState's exited-word set treats "discontinued" and
// "notfound" statuses as exited.
func (c *Client) GetMachine(ctx context.Context, providerID string) (provider.RunningMachine, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return provider.RunningMachine{}, fmt.Errorf("verda: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/instances/"+providerID, nil)
	if err != nil {
		return provider.RunningMachine{}, fmt.Errorf("verda: build the request for instance %s: %w", providerID, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return provider.RunningMachine{}, fmt.Errorf("verda: could not be reached for instance %s: %w", providerID, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return provider.RunningMachine{}, fmt.Errorf("verda: instance %s: %w", providerID, provider.ErrMachineNotFound)
	}
	if resp.StatusCode >= 400 {
		return provider.RunningMachine{}, fmt.Errorf("verda: refused instance %s, status %d: %s", providerID, resp.StatusCode, provider.Truncate(raw, 300))
	}
	var row runningInstance
	if err := json.Unmarshal(raw, &row); err != nil {
		return provider.RunningMachine{}, fmt.Errorf("verda: instance %s answered with something that is not the instance shape: %w", providerID, err)
	}
	return rowToRunningMachine(row), nil
}

// imageType is one row of GET /v1/images, holding only the fields chooseHostImage reads. It never
// reads `name`, which is spelled differently from the `image_type` field the create takes. [DOC]
// The OpenAPI spec's Image schema names `id`, `name`, `image_type`, `category`, `details`.
type imageType struct {
	Category  string   `json:"category"`
	ImageType string   `json:"image_type"`
	Details   []string `json:"details"`
}

// cuda13MinorPattern finds "CUDA 13.<minor>" inside one of an image's `details` strings, per the
// image rule on chooseHostImage.
var cuda13MinorPattern = regexp.MustCompile(`CUDA 13\.(\d+)`)

// chooseHostImage asks GET /v1/images?instance_type=<shapeCode> and picks the newest Ubuntu 24.04
// CUDA 13.x Docker image Verda offers for that shape: category "docker", details naming both
// "Ubuntu 24.04" and a "CUDA 13.<minor>" entry, highest minor wins, ties by first listed.
// vLLM's official `:latest` tag needs CUDA 13.0.3's driver floor (>=580); the newest image gives
// headroom for the day `:latest` moves its CUDA version again. Nothing matching refuses by name,
// listing every image_type Verda did offer, rather than falling back to a guess.
func (c *Client) chooseHostImage(ctx context.Context, shapeCode string) (string, error) {
	if shapeCode == "" {
		return "", fmt.Errorf("verda: no shape to choose a host image for")
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("verda: %w", err)
	}
	var images []imageType
	if err := c.getJSON(ctx, token, "/images", url.Values{"instance_type": {shapeCode}}, &images); err != nil {
		return "", fmt.Errorf("verda: list images for %s: %w", shapeCode, err)
	}
	best := ""
	bestMinor := -1
	offered := make([]string, 0, len(images))
	for _, img := range images {
		offered = append(offered, img.ImageType)
		if img.Category != "docker" {
			continue
		}
		hasUbuntu2404 := false
		minor := -1
		for _, d := range img.Details {
			if strings.Contains(d, "Ubuntu 24.04") {
				hasUbuntu2404 = true
			}
			if m := cuda13MinorPattern.FindStringSubmatch(d); m != nil {
				if v, convErr := strconv.Atoi(m[1]); convErr == nil {
					minor = v
				}
			}
		}
		if hasUbuntu2404 && minor > bestMinor {
			bestMinor = minor
			best = img.ImageType
		}
	}
	if best == "" {
		return "", fmt.Errorf("verda: no Ubuntu 24.04 CUDA 13.x Docker image is offered for %s; Verda offered: %s", shapeCode, strings.Join(offered, ", "))
	}
	return best, nil
}

// DescribeServe asks GET /v1/images to choose a host image for o.ShapeCode, then reports the
// image, the disk sizes and the exact vllm serve arguments and env key names a serving
// CreateMachine call for o will send. The console calls it to write the claim record before the
// instance is asked for. o.Format "gguf" never reaches here: ServesFormat refuses it before the
// console calls this.
func (c *Client) DescribeServe(ctx context.Context, o provider.ServeOptions) (provider.ServeDescription, error) {
	hostImage, err := c.chooseHostImage(ctx, o.ShapeCode)
	if err != nil {
		return provider.ServeDescription{}, err
	}
	containerGB, persistentGB := provider.ServingDiskSizes(o.DiskBytes)
	return provider.ServeDescription{
		ImageOrTemplate:  hostImage + " + vllm/vllm-openai:latest",
		HostImage:        hostImage,
		ContainerDiskGB:  containerGB,
		PersistentDiskGB: persistentGB,
		Args:             provider.VLLMServeArgs(o),
		EnvKeys:          provider.VLLMServeEnvKeys(o),
	}, nil
}

// buildStartupScript builds the bash script that writes the machine's key to a 0600 env file,
// hands `docker pull` with a retry loop and `docker run` of the official vLLM image to a
// transient systemd unit (systemd-run, so the ~9 GiB pull never sits under the startup script's
// start timeout, [DOC] forum.verda.com/t/timed-out-start-script/38), and keeps the container's
// output in /var/log/gpugate-serve.log. ASCII only. The key is written to the env file only,
// never a `-e` flag and never logged. The function is unexported and DescribeServe never returns
// its text, because it holds the key.
func buildStartupScript(o provider.CreateOptions) string {
	serve := o.Serve
	envLines := []string{"'VLLM_API_KEY=" + serve.APIKey + "'"}
	if serve.HFToken != "" {
		// Same file, same reason as the machine's key: never a `-e` flag, never logged. "" means no
		// token configured, so no line is written and the download proceeds unauthenticated.
		envLines = append(envLines, "'HF_TOKEN="+serve.HFToken+"'")
	}
	if serve.GPUCount > 1 {
		envLines = append(envLines, "'NCCL_DEBUG=INFO'", "'NCCL_P2P_DISABLE=1'")
	}
	args := provider.VLLMServeArgs(*serve)

	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("# GPUGate serving bootstrap for " + o.Hostname + ": runs once, as root, at first boot.\n")
	b.WriteString("set -eu\n")
	b.WriteString("install -d -m 0700 /var/lib/gpugate /var/lib/gpugate/hf\n")
	b.WriteString("umask 077\n")
	b.WriteString("printf '%s\\n' " + strings.Join(envLines, " ") + " > /var/lib/gpugate/serve.env\n")
	b.WriteString("cat > /var/lib/gpugate/serve.sh <<'GPUGATE_SERVE'\n")
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -u\n")
	b.WriteString("exec >> /var/log/gpugate-serve.log 2>&1\n")
	b.WriteString("echo \"$(date -u +%FT%TZ) gpugate: pulling vllm/vllm-openai:latest\"\n")
	b.WriteString("until docker pull vllm/vllm-openai:latest; do echo \"$(date -u +%FT%TZ) gpugate: pull failed, retry in 15s\"; sleep 15; done\n")
	b.WriteString("docker image inspect --format 'gpugate: image {{.Id}} {{index .RepoDigests 0}}' vllm/vllm-openai:latest\n")
	b.WriteString("echo \"$(date -u +%FT%TZ) gpugate: starting the container\"\n")
	b.WriteString("docker run -d --name gpugate-serve --restart no --runtime nvidia --gpus all --ipc=host \\\n")
	b.WriteString("  -p 127.0.0.1:8000:8000 -v /var/lib/gpugate/hf:/root/.cache/huggingface \\\n")
	b.WriteString("  --env-file /var/lib/gpugate/serve.env \\\n")
	b.WriteString("  vllm/vllm-openai:latest " + args + "\n")
	b.WriteString("docker logs -f gpugate-serve\n")
	b.WriteString("echo \"$(date -u +%FT%TZ) gpugate: container exited with $(docker inspect --format '{{.State.ExitCode}}' gpugate-serve)\"\n")
	b.WriteString("GPUGATE_SERVE\n")
	b.WriteString("chmod 0700 /var/lib/gpugate/serve.sh\n")
	b.WriteString("systemd-run --unit=gpugate-serve --collect /var/lib/gpugate/serve.sh\n")
	return b.String()
}

// CreateStartupScript creates one Verda startup script for o and answers its id, implementing the
// console's startupScripter interface. [DOC] https://api.verda.com/v1/openapi.json,
// `POST /v1/scripts`, AddScriptDto: {name, script}, both required. Answers 201; the id is read
// with parseCreateInstanceID, the tolerant reader CreateMachine's instance id uses, because the
// response is documented only as an "example" quoted string (see CreateMachine for why that is
// not trusted at face value). Called before the instance is asked for: the script must already
// exist, referenced by id, before a create can name it.
func (c *Client) CreateStartupScript(ctx context.Context, o provider.CreateOptions) (string, error) {
	if o.Serve == nil {
		return "", fmt.Errorf("verda: no model to serve, nothing to script")
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("verda: %w", err)
	}
	body, err := json.Marshal(map[string]string{
		"name":   "gpugate-" + o.Hostname,
		"script": buildStartupScript(o),
	})
	if err != nil {
		return "", fmt.Errorf("verda: build the script create request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/scripts", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("verda: build the script create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("verda: could not be reached to create the startup script for %s: %w", o.Hostname, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		// respText: whether Verda ever echoes a submitted script body back in a validation error
		// is [UNKNOWN], and that body is the one place VLLM_API_KEY and HF_TOKEN exist together
		// in cleartext (buildStartupScript). It is redacted before it reaches the log, the way
		// the same helper guards RunPod's create refusal.
		respText := provider.RedactSecrets(provider.Truncate(raw, 300), o.Serve)
		return "", fmt.Errorf("verda: refused to create the startup script for %s, status %d: %s", o.Hostname, resp.StatusCode, respText)
	}
	id, ok := parseCreateInstanceID(raw)
	if !ok {
		return "", fmt.Errorf("verda: created the startup script for %s but answered with something that could not be read as an id: %s", o.Hostname, provider.Truncate(raw, 300))
	}
	return id, nil
}

// DeleteStartupScript deletes scriptID. [DOC] https://api.verda.com/v1/openapi.json,
// `DELETE /v1/scripts/{scriptId}`, answers 200. Called by the console once the claim that used it
// leaves 'loading': a failed delete is logged and left for the next resumed poll to retry.
func (c *Client) DeleteStartupScript(ctx context.Context, scriptID string) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return fmt.Errorf("verda: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/scripts/"+scriptID, nil)
	if err != nil {
		return fmt.Errorf("verda: build the script delete request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("verda: could not be reached to delete the startup script %s: %w", scriptID, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("verda: refused to delete the startup script %s, status %d: %s", scriptID, resp.StatusCode, provider.Truncate(raw, 300))
	}
	return nil
}

// CreateMachine asks Verda to rent one instance of o.ShapeCode in o.Locations[0]. Verda's create
// takes exactly one `location_code`, never a list, so this refuses with "Verda needs one
// location" when o.Locations is empty rather than guessing one. [DOC]
// https://api.verda.com/v1/openapi.json, `POST /v1/instances`, DeployInstancePublicDto: its
// required fields are instance_type, image, hostname, description and location_code, all five
// sent here (image is o.Serve.HostImage, the console's copy of DescribeServe's choice). Every
// launch names a model, so o.Serve is never nil here; it is refused by name defensively. An empty
// HostImage or StartupScriptID (both set before this is called: the script is created first, then
// the instance) is refused the same way, because it would be a bug in this program and is never
// sent to Verda to find out what it does with one.
//
// `startup_script_id` names the script CreateStartupScript already created for this launch.
// `os_volume` is sized from o.Serve.DiskBytes the way RunPod's disk and mounts.persistent are
// (provider.ServingDiskSizes): one volume holds the OS, the pulled image and the weights on
// Verda, so the two RunPod figures are summed.
//
// `ssh_key_ids` carries the ids of every SSH key this account holds, and both leaving it out and
// sending it empty break the call. [OBS] Leaving the field out answered "SSH keys can be an array
// of UUID's, a single UUID string, null value or not defined", and sending it as null answered
// "SSH keys must be provided if image is not an OS volume". So Verda wants real key ids for an
// image like this one. They are read from the account at create time rather than configured; a
// key id is a reference and not a secret, and no key material is read. An account with no key at
// all still sends null, which is as far as this can go without one.
//
// [DOC] https://api.verda.com/v1/openapi.json describes the 202 body only as an "example", with
// no schema: `"d37f0444-f0a2-435b-a356-1b5f66116bec"`, which renders as a quoted JSON string but
// settles nothing about the wire shape. [OBS] A real create answers 202 with the instance id as
// bare, UNQUOTED text, which is not valid JSON, and the machine is already created and billing.
// parseCreateInstanceID accepts every shape Verda is known or documented to use: a quoted JSON
// string, that bare unquoted id, and a JSON object carrying the id under a plausible key. When
// none of those parse, this is never read as a refusal (nothing 4xx was seen): it is
// provider.ErrCreateOutcomeUnknown, and the caller must leave the claim open for the watcher to
// reconcile against a fresh answer. On a parsed id, this then reads that instance back once by
// id, so the caller gets the shape a fresh ListMachines call would answer rather than an object
// this package assembled from what it asked for.
func (c *Client) CreateMachine(ctx context.Context, o provider.CreateOptions) (provider.RunningMachine, error) {
	if len(o.Locations) == 0 {
		return provider.RunningMachine{}, fmt.Errorf("verda: Verda needs one location")
	}
	if o.Serve == nil {
		return provider.RunningMachine{}, fmt.Errorf("verda: Verda serves through vLLM only; a create with no model is refused")
	}
	if o.Serve.HostImage == "" {
		return provider.RunningMachine{}, fmt.Errorf("verda: no host image was chosen for this launch")
	}
	if o.Serve.StartupScriptID == "" {
		return provider.RunningMachine{}, fmt.Errorf("verda: no startup script was created for this launch")
	}
	location := o.Locations[0]
	token, err := c.accessToken(ctx)
	if err != nil {
		return provider.RunningMachine{}, fmt.Errorf("verda: %w", err)
	}
	var sshKeyIDs any
	if ids, keysErr := c.sshKeyIDs(ctx, token); keysErr != nil {
		return provider.RunningMachine{}, fmt.Errorf("verda: %w", keysErr)
	} else if len(ids) > 0 {
		sshKeyIDs = ids
	}
	containerGB, persistentGB := provider.ServingDiskSizes(o.Serve.DiskBytes)
	body, err := json.Marshal(map[string]any{
		"instance_type":     o.ShapeCode,
		"image":             o.Serve.HostImage,
		"hostname":          o.Hostname,
		"description":       o.Hostname,
		"location_code":     location,
		"ssh_key_ids":       sshKeyIDs,
		"startup_script_id": o.Serve.StartupScriptID,
		"os_volume":         map[string]any{"name": o.Hostname, "size": containerGB + persistentGB},
	})
	if err != nil {
		return provider.RunningMachine{}, fmt.Errorf("verda: build the create request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/instances", strings.NewReader(string(body)))
	if err != nil {
		return provider.RunningMachine{}, fmt.Errorf("verda: build the create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return provider.RunningMachine{}, fmt.Errorf("verda: could not be reached to create %s, so the request may or may not have arrived: %w: %w", o.ShapeCode, err, provider.ErrCreateOutcomeUnknown)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return provider.RunningMachine{}, fmt.Errorf("verda: refused to create %s in %s, status %d: %s", o.ShapeCode, location, resp.StatusCode, provider.Truncate(raw, 300))
	}
	instanceID, ok := parseCreateInstanceID(raw)
	if !ok {
		// This status was already checked above and is not a refusal (< 400), so the machine
		// may already exist and may already be billing. Never settle this as a plain refusal:
		// provider.ErrCreateOutcomeUnknown tells the caller to leave the claim open instead.
		return provider.RunningMachine{}, fmt.Errorf("verda: answered the create for %s with something that could not be told apart from a refusal: %s: %w", o.ShapeCode, provider.Truncate(raw, 300), provider.ErrCreateOutcomeUnknown)
	}

	// The machine now exists and is billing. From here, a failure to read it back is reported,
	// never treated as the create having failed: the caller must still record the id it got.
	var row runningInstance
	if err := c.getJSON(ctx, token, "/instances/"+instanceID, nil, &row); err != nil {
		return provider.RunningMachine{ProviderKey: providerKey, ProviderID: instanceID, ShapeCode: o.ShapeCode, Hostname: o.Hostname, State: "created, not yet read back"},
			fmt.Errorf("verda: created %s (%s) but could not read it back: %w", o.ShapeCode, instanceID, err)
	}
	return rowToRunningMachine(row), nil
}

// DestroyMachine asks Verda to delete the instance addressed by providerID. [DOC]
// https://api.verda.com/v1/openapi.json, `PUT /v1/instances`, PerformInstanceActionPublicDto:
// {"action": "delete", "id": "<uuid>"}. No `volume_ids` is sent, so Verda's default applies:
// "only the OS volume will be deleted and the rest detached", never a second volume this program
// did not attach. A single-id call answers 202 (or 204 when the instance is already in the
// requested state) on success, 400 or 404 on refusal.
func (c *Client) DestroyMachine(ctx context.Context, providerID string) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return fmt.Errorf("verda: %w", err)
	}
	body, err := json.Marshal(map[string]string{"action": "delete", "id": providerID})
	if err != nil {
		return fmt.Errorf("verda: build the destroy request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+"/instances", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("verda: build the destroy request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("verda: could not be reached to destroy %s: %w", providerID, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("verda: refused to destroy %s, status %d: %s", providerID, resp.StatusCode, provider.Truncate(raw, 300))
	}
	return nil
}

// accessToken returns a cached bearer token, fetching a fresh one when none is held or the held
// one is close to expiry. [DOC] The token's expires_in is 3600 seconds; this refreshes at 3000 to
// leave margin.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenUntil) {
		return c.token, nil
	}

	body, err := json.Marshal(map[string]string{
		"grant_type":    "client_credentials",
		"client_id":     c.clientID,
		"client_secret": c.clientSecret,
	})
	if err != nil {
		return "", fmt.Errorf("build the token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/oauth2/token", strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("build the token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not be reached for the oauth2 token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("refused the oauth2 token, status %d: %s", resp.StatusCode, provider.Truncate(raw, 300))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("answered the oauth2 token with something that is not JSON: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("answered the oauth2 token with no access_token")
	}
	c.token = out.AccessToken
	expiresIn := out.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	c.tokenUntil = time.Now().Add(time.Duration(expiresIn-600) * time.Second)
	return c.token, nil
}

func (c *Client) getJSON(ctx context.Context, token, path string, query url.Values, out any) error {
	u := baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build the request for %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not be reached for %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("refused %s, status %d: %s", path, resp.StatusCode, provider.Truncate(raw, 300))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("answered %s with something that is not JSON: %w", path, err)
	}
	return nil
}

// parseCreateInstanceID reads what POST /v1/instances answered on a non-refusal status and
// returns the instance id it names, whichever shape Verda used. See CreateMachine for what is
// [DOC] and what is [OBS] here. It tries, in order: a quoted JSON string; a JSON object carrying
// the id under "id" or "instance_id"; and finally bare unquoted text, accepted only when it has
// the shape of a UUID, which is the one confirmed [OBS] real case.
func parseCreateInstanceID(raw []byte) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", false
	}
	var quoted string
	if err := json.Unmarshal(trimmed, &quoted); err == nil && quoted != "" {
		return quoted, true
	}
	var obj struct {
		ID         string `json:"id"`
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(trimmed, &obj); err == nil {
		if obj.ID != "" {
			return obj.ID, true
		}
		if obj.InstanceID != "" {
			return obj.InstanceID, true
		}
	}
	if looksLikeUUID(string(trimmed)) {
		return string(trimmed), true
	}
	return "", false
}

// looksLikeUUID reports whether s has the standard 8-4-4-4-12 hyphenated hex shape every Verda
// instance id observed so far uses, so bare unrecognisable text is never mistaken for one.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// sshKeyIDs answers the ids of the SSH keys this account holds. [DOC]
// https://api.verda.com/v1/openapi.json, `GET /v1/sshkeys`. Only the id and the name of a key
// are ever read; the key material itself is never fetched, printed or stored.
func (c *Client) sshKeyIDs(ctx context.Context, token string) ([]string, error) {
	var rows []struct {
		ID string `json:"id"`
	}
	if err := c.getJSON(ctx, token, "/sshkeys", nil, &rows); err != nil {
		return nil, fmt.Errorf("read the account's ssh keys: %w", err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.ID != "" {
			ids = append(ids, row.ID)
		}
	}
	return ids, nil
}

// ListSSHPublicKeys answers the "key" field of every SSH key this account holds, in OpenSSH
// authorized_keys form, for the console's launch-time check that the private key at
// sshtunnel.PrivateKeyPath is one of them. [DOC] https://api.verda.com/v1/openapi.json,
// GetKeysResponseDto.key, "Public SSH key", required. It never fetches private key material.
func (c *Client) ListSSHPublicKeys(ctx context.Context) ([]string, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("verda: %w", err)
	}
	var rows []struct {
		Key string `json:"key"`
	}
	if err := c.getJSON(ctx, token, "/sshkeys", nil, &rows); err != nil {
		return nil, fmt.Errorf("verda: read the account's ssh keys: %w", err)
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Key != "" {
			lines = append(lines, row.Key)
		}
	}
	return lines, nil
}
