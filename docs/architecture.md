# Architecture

How GPUGate is put together and why. The code is the source of truth for every detail: this page says what each part is for and where to find it, and the comments in the code say how it works.

## Contents

- [What GPUGate is](#what-gpugate-is)
- [The parts](#the-parts)
- [One program, one port](#one-program-one-port)
- [The login](#the-login)
- [Commands](#commands)
- [The life of a rental](#the-life-of-a-rental)
- [What runs in the background](#what-runs-in-the-background)
- [The fixed address](#the-fixed-address)
- [Moving to a cheaper machine](#moving-to-a-cheaper-machine)
- [Will the model fit](#will-the-model-fit)
- [The console page](#the-console-page)
- [What is stored](#what-is-stored)
- [Settings and credentials](#settings-and-credentials)
- [Principles the design holds to](#principles-the-design-holds-to)
- [What is deliberately not here](#what-is-deliberately-not-here)

## What GPUGate is

A single Go program you run on your own computer. It lists the GPU machines RunPod and Verda will rent, says which of them can serve a chosen model at a chosen context window and number of parallel chats, rents one with a click, and has it come up already serving that model. It shows what is running on both accounts and destroys a machine with a click or on its own when the machine has been idle too long.

Each project you create gets a fixed local address, `/p/<project>/v1`, that speaks the OpenAI and Anthropic APIs. A coding tool or a benchmark points at that address and never needs to know which machine is behind it, where it is, or its key.

## The parts

| Path | What it is for |
| --- | --- |
| `cmd/gpugate/` | Starts the program: loads the settings file, the model catalog and the database, builds the provider clients, and listens. Also the `list`, `destroy`, `set-password` and `make-key` commands. |
| `internal/console/` | The server itself: the HTTP routes, the background loops, launching, destroying, the fixed address, and the cheaper-machine mover. |
| `internal/provider/` | What every provider has in common: the interface each adapter implements, the vLLM command line, disk sizing, card facts, EU and US location sets, and the shared HTTP client. |
| `internal/provider/runpod/`, `internal/provider/verda/` | One adapter per provider. |
| `internal/fit/` | The memory arithmetic that decides whether a model fits a machine. |
| `internal/catalog/` | Reads `catalog/models.json`, the list of models GPUGate can serve. |
| `internal/store/` | The SQLite database and its schema (`schema.sql`). |
| `internal/login/` | The password, the sessions and the access key that guard every route. |
| `internal/podkey/` | One random API key per rented machine, kept in a file, never in the database. |
| `internal/sshtunnel/` | The SSH connection to each Verda machine (see [providers](providers.md#verda)). |
| `internal/destroyer/` | The one destroy, used by the console and by the `destroy` command. |
| `internal/logring/`, `internal/envfile/` | The in-memory log the console shows, and the reader for the settings file. |
| `ui/` | The console page: `index.html`, the source in `src/`, the built copy in `dist/`, and htmx and the IBM Plex fonts in `vendor/`. |
| `catalog/models.json` | The models, each with the figures the fit needs and the basis of each figure. |
| `gate.sh` | Build, start, stop, restart, status and log. The binary goes to `dist/gpugate`. |

## One program, one port

GPUGate is one binary, one port and one running copy. The port is `GPUGATE_PORT` (default 8719) and the addresses are `GPUGATE_LISTEN`, a list of IP addresses (default loopback only). It binds exactly those: a hostname, `0.0.0.0` and `::` are refused, so it never listens on every interface. If the port is taken it stops and says so rather than moving to another port, so there is never a second copy answering somewhere unexpected. `gate.sh` keeps the process id in a file and only ever stops that one process.

The listen list is the first line of protection and the [login](#the-login) is the second.

## The login

`internal/login` wraps the whole route table, so every route is refused unless the wrapper names it open or the request carries a valid session. A route added later is guarded without anyone remembering to. Open without a session: `/login`, `/logout` and the favicon. Page visits (`/` and `/console`) are sent to `/login`; every other refusal is a 401 the page and the tools can read.

- **The password** is set by `gpugate set-password` (at least 12 characters; it creates `.state` if missing and needs no database) and stored as an Argon2id hash at the minimum OWASP gives, in `.state/login-password-hash`, a file and not a database column because the database is copied into snapshots. Every cost parameter is inside the stored string and bounded when it is read. The running program re-reads the file, so a new password ends every session. With no password set nothing but the login page answers, and that page says to run the command.
- **Sessions** are random tokens in a cookie that script cannot read and that is not sent on cross-site requests. They live in memory, so a restart logs the browser out, and each lasts 30 days from login. Every POST, PUT and DELETE made with a session must carry an `Origin` (or `Referer`) naming the request's own host, which stops another web page from driving the console through a logged-in browser. Wrong passwords and wrong access keys are counted per source address, taken from the connection and never from a forwarded header: five in a row lock that address for 60 seconds, failures older than 15 minutes are forgotten, and 50 from anywhere in 10 minutes lock everyone for 60 seconds. Because the count is per address, one source's wrong guesses do not lock out a user at another address.
- **The Host header** must name a listen address, `localhost` or a name in `GPUGATE_HOSTNAMES`, otherwise the answer is 421. This stops a web page reaching the program through a name it controls (DNS rebinding). When no access key is set, a write to the fixed address that carries a foreign `Origin` is refused; tools send none.
- **The access key** (`GPUGATE_ACCESS_KEY`) is for tools, which cannot type a password. When it is set (at least 32 characters, or the program does not start; `gpugate make-key` prints one), `/p/...` and `GET /api/projects` accept it as a bearer token or `X-Api-Key`, or a valid session; the console page itself calls `GET /api/projects`, so a session must work there too. When it is not set those two stay open, and the start log says so. Because the key is optional, tools can be changed to send it first and the key switched on after, so nothing breaks in between.
- **The fixed address never passes the caller's credentials on.** The caller's `Authorization`, `X-Api-Key` and `Cookie` are dropped before the request goes to a rented machine, whose own key is put in instead, and a `Set-Cookie` in the machine's answer is dropped. The machine's own `Content-Security-Policy` and `Access-Control-*` headers are dropped too and every answer gets `Content-Security-Policy: sandbox`, because it is served from the console's origin and must not run as part of it.
- **The page's own files** are served from an allow list (the page, the favicon, `dist/` and `vendor/`), never as a directory listing, and only behind a session. Every answer carries `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff` and `Referrer-Policy: same-origin`, API answers are `no-store`, and the console and login pages carry a content security policy naming only what they load. The console page's policy allows only the program's own address, because its fonts are served from `ui/vendor/fonts`. The server gives a client 10 seconds to send its headers, the login form 10 seconds to arrive, and an idle connection 2 minutes; it sets no global read or write timeout because the fixed address streams.
- **The page** sends the browser to `/login` on any 401 and stops its polls.

The login is plain HTTP and there is no second factor. It stops another person on the network and another web page; it does not stop someone who can read the traffic, and the program warns at start about a listen address that is not loopback or in Tailscale's address ranges (`trustedAddress` in `cmd/gpugate/main.go`). Tailscale's IPv4 range is also carrier-grade NAT space, so an address in it is not always a Tailscale one. `GPUGATE_COOKIE_SECURE=1` marks the cookie Secure for a TLS front.

## Commands

`gpugate list` prints every machine on both accounts, or says why an account could not be asked, never "none". `gpugate destroy <provider> <id>` writes the destroy intent first and then runs the same destroy the console runs (`internal/destroyer/`): it asks until the provider accepts, waits for a fresh list without the machine, then closes the record. Both run from the project folder and refuse to run elsewhere, so they never create an empty database. They are for destroying a machine when the console is not at hand, and they never start the server's background loops. `gpugate set-password` and `gpugate make-key` need neither the database nor the providers and are covered under [the login](#the-login). The database is opened with a busy timeout, so a command and a running server wait for each other's writes.

## The life of a rental

1. **Launch.** A launch from the console is checked again on the server against a fresh provider answer: the model fits, the provider can serve its format, the machine is ticked as allowed, the price is current, and some location can supply it. A refusal is reported in plain words before anything is spent.
2. **The record comes first.** A claim row is written to the database before the provider is asked for anything, so a crash at any moment still leaves a record of what may be billing.
3. **Create.** The provider is asked to create a machine that boots straight into serving the model, with a fresh random key and every serving setting on its command line. Nothing is installed by hand afterwards.
4. **Confirmed.** The machine counts as running only once the provider's own list of machines shows it.
5. **Loading, then serving.** GPUGate asks the machine for its list of models every few seconds. The machine is serving once the model appears there.
6. **In use.** Requests reach it through the [fixed address](#the-fixed-address), which also counts tokens and keeps the idle clock.
7. **Ending.** A machine is destroyed when you press Delete, when it has been quiet for its idle limit, when it never manages to serve within the hour, when it hangs, or when the mover has replaced it. Every destroy records its intent first, retries until the provider accepts, and is only marked gone once a fresh provider list no longer shows the machine.

A claim's row keeps its last state for good. Whether a machine is alive is always decided from a fresh provider list, never from the row.

## What runs in the background

- **The live poll** asks each provider what is running every 20 seconds. It matches machines to claims, starts watching new ones, applies the idle limit, keeps the Verda SSH connections up, and saves token counts. If a provider cannot be asked, its last good answer is kept for the fixed address and the page says that provider could not be asked, never that nothing is running.
- **The readiness watch** follows each loading machine until it serves, gives up after one hour, and destroys a machine that exits or vanishes before serving.
- **The hang watch** destroys a loading machine whose log has shown nothing but vLLM's two-card hang message for ten minutes (see [providers](providers.md#multi-card-machines)).
- **The destroyer** keeps asking until the provider accepts, then waits for a fresh list without the machine.
- **The mover** looks for a cheaper machine every three minutes, on projects that have it switched on.
- **On start**, GPUGate picks up where it left off: unfinished destroys, loading machines and moves all resume. A machine that outlived the program keeps running and is found again.

## The fixed address

`/p/<project>/...` forwards each request to that project's serving machine, swapping in the machine's own key so the caller never holds it. A request names a model by the name the machine serves, which is the catalog row's Hugging Face repository (`hf_repo`). When a project has more than one machine serving, the request goes to the newest one serving that model, which is how a move switches over with no gap.

On the way through, GPUGate counts tokens in, out and read from cache, and records when output last flowed, which is what the idle limit measures. It also smooths over two engine quirks (see [providers](providers.md#model-servers)). Every outgoing request carries the same plain browser User-Agent.

`GET /api/projects` lists every project and the models each is serving, for tools that pick a project before connecting. Both are guarded by the access key when one is set (see [the login](#the-login)).

## Moving to a cheaper machine

A project can have auto-move switched on. Then GPUGate keeps looking for a cheaper machine, among the ones you ticked, that holds the same model at the same context and chat count. It rents it, waits until it serves, switches the project's address to it, and destroys the old machine once the new one has answered cleanly for a minute and nothing is still running on the old one. It never picks a different model or setting, and never a machine you did not tick, and never one over the price cap. Pressing Launch on such a project, or on a machine with more cards than needed, first offers the cheaper choice.

## Will the model fit

`internal/fit` works out whether a model fits a machine from the model's weights, its key-value cache per token, the context and the number of chats, against the whole machine's memory: every card added together. One card's memory and the whole machine's are never mixed. The catalog row carries the figures and where each came from. The arithmetic does not count the recurrent state of hybrid models, and cannot size a sliding-window model from one constant (see [providers](providers.md#model-servers)).

## The console page

Plain JavaScript draws and sorts the machine table, filters and ticks, draws the project list, and drives the launch panel. htmx loads the parts the server renders, the running strip and the sessions list, and posts the new-project form. There is no framework and no bundler; `ui/src` is copied to `ui/dist` by `npm --prefix ui run build`, the built copy is committed, and CI fails when the two differ.

The page has Machines (what is running, what to run, the machine table), Projects, and Sessions (every rental with its time, cost and tokens). A log drawer shows what the program is doing. The routes the page calls are registered in `internal/console/server.go`.

## What is stored

`.state/gpugate.db` holds the ticks, the projects, the last provider answers, one row per rental (the claims), saved per-model settings, destroy intents and moves. `internal/store/schema.sql` says what each table is for. Machine keys live in `.state/pod-keys/`, one file per machine, and are deleted when the machine is gone. None of this is committed.

## Settings and credentials

Credentials come from the environment or from a `.env` file in the working directory; a variable already set in the environment wins. The names are `RUNPOD_API_GPU_GATE` (RunPod), `VERDA_MAIN_CLIENT_ID` and `VERDA_MAIN_CLIENT_SECRET` (Verda), `HF_API_KEY` or `HF_TOKEN` (Hugging Face, optional), `GPUGATE_ACCESS_KEY` (optional, the key tools send to the fixed address), `GPUGATE_HOSTNAMES` and `GPUGATE_COOKIE_SECURE` (optional, see [the login](#the-login)). The database only ever stores where a secret lives, never its value. `GPUGATE_LISTEN` and `GPUGATE_PORT` set where it listens (see [One program, one port](#one-program-one-port)). [`example.env`](../example.env) names every variable the program reads. The price cap is not an environment variable: it is one value in the database's `setting` table, set under Projects in the console. It is US dollars per hour for the whole program, empty means no cap, and it is enforced when a launch is checked and when the auto-move picks a machine. A machine whose price the provider did not name is refused while a cap is set. A machine already running is not touched when the cap is lowered. The paths are constants.

## Principles the design holds to

- **Only a fresh provider answer says what is running.** Our own notes say what we did. "Could not find out" is its own answer and is never shown as "nothing is running".
- **The record is written before money is spent**, and a restarted program finishes or stops whatever was under way.
- **The program never chooses for you.** It never picks a model, a setting or a machine, and a provider's refusal is reported, not quietly worked around. The mover is the one exception, and only within what you ticked.
- **Images are never rebuilt.** Models are downloaded onto the rented machine and every serving setting goes on the command line, so changing a model or a flag is a change here, never an image change.

## What is deliberately not here

No spending total or budget (only the hourly price cap), no spot machines, no queue that waits for capacity, no changing the model on a running machine, and no coordination between two launches made at once.
