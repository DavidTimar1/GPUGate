-- Every table GPUGate keeps. The file is applied on every start with CREATE TABLE IF NOT EXISTS,
-- which leaves an existing table untouched: a column added here later reaches an existing
-- database only through a migration written for it (see store.Open).
--
-- `setting` holds small named values: the hourly price cap and the moment the Sessions view was
-- last cleared.
CREATE TABLE IF NOT EXISTS setting (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- `shape_tick` holds the shape codes that are ticked, account-wide: one gate over what may be
-- rented at all, never a per-project budget. Ticking a shape once makes it launchable from any
-- project.
CREATE TABLE IF NOT EXISTS shape_tick (
    shape_code TEXT PRIMARY KEY,
    ticked_at  TEXT NOT NULL
);

-- `project` names every project a claim can be launched for, each with its own fixed address
-- `/p/<slug>/...`. `coding` is seeded the moment a database is opened (store.Open) and can never
-- be deleted. A slug is 1 to 32 characters of lowercase ASCII letters, digits and dashes, starting
-- with a letter (store.ValidProjectSlug), checked before a row reaches this table, so nothing here
-- can hold a slug the fixed address could not route. `auto_move` is the project's switch for
-- moving its serving model to a cheaper machine by itself, 0 (off) until the user turns it on.
CREATE TABLE IF NOT EXISTS project (
    slug       TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    auto_move  INTEGER NOT NULL DEFAULT 0
);

-- `machine` mirrors the last fresh provider answer for what is running on that provider's
-- account. Our notes say what we DID, so this table is read only when a provider could not be
-- asked just now, and it is never the thing that decides what IS running: a fresh answer always
-- overwrites it. Every row is replaced, never patched, on a successful poll of its provider, so
-- the whole table always reflects one provider's last answer.
CREATE TABLE IF NOT EXISTS machine (
    provider_key    TEXT NOT NULL,
    provider_id     TEXT NOT NULL,
    shape_code      TEXT NOT NULL,
    display_name    TEXT NOT NULL,
    gpu_name        TEXT NOT NULL,
    location        TEXT NOT NULL,
    price_per_hour  REAL,
    state           TEXT NOT NULL,
    last_seen_at    TEXT NOT NULL,
    PRIMARY KEY (provider_key, provider_id)
);

-- `machine_claim` is a launch's record: written and committed the instant Launch is pressed,
-- before the provider is asked to create anything. A process that dies between the write and the
-- provider's answer leaves this row saying exactly what was being rented, so the watcher (the live
-- poll, reconciling what it hears against every open claim) can find out what really happened.
-- `hostname` is the name this program gives the machine at create time, and is how a claim whose
-- create call was never answered is matched back to a machine a later fresh answer reports.
-- `state` is 'asked' until the provider answers, then 'created' (a provider_id exists, not yet
-- confirmed by a fresh poll), 'confirmed' (a fresh poll has seen it), 'refused' (the provider said
-- no, nothing was made), or 'abandoned' (over an hour old, no machine ever showed up and none is
-- being waited on any more).
--
-- `project` names which project this claim belongs to: `/p/<project>/...` routes a request to a
-- serving machine only among claims that carry its project name.
--
-- `model_name`, `image_or_template`, `serve_args` and `serve_env_keys` name what a claim asked to
-- be served, written before the create, so the record says what was asked for and not only what
-- machine. `serve_env_keys` holds the NAMES of the environment variables the create sends, comma
-- separated, and never a value: no column here may ever hold a credential. `serving_state` is ''
-- until a fresh poll confirms the machine, then 'loading' while the readiness poll runs, then
-- 'serving' once the model's model list answers (never on this program's say-so). A 'loading'
-- claim whose machine turns out to be gone (no key on file, the machine exited, or a direct read
-- finds it not found) is settled 'gave_up' instead, so a restart never resumes waiting on it; the
-- one-hour ceiling gives up without this, because that machine is left running and a restart
-- resuming its poll is right. `loading_since` and `serving_since` are when each state was
-- entered, so a restart can tell how long a machine has been waiting.
--
-- The inactivity timeout: `last_output_at` is the moment the fixed address last forwarded a byte
-- back from this claim's machine, flushed once per live-poll pass from an in-memory clock the
-- proxy moves (never written on the proxy's hot path). NULL until the first proxied request. The
-- sweep's quiet-since is the latest of this and `serving_since` (see console.quietSince).
--
-- `idle_minutes` is this machine's idle timeout: the inactivity sweep destroys it once its
-- quiet-since is this many minutes old, 0 meaning never. Set at launch (default 30), changed from
-- the strip, inherited by an auto-move's new machine.
--
-- `tokens_in`, `tokens_out` and `tokens_cached` are this claim's running totals of tokens read out
-- of every answer the fixed address forwarded for it: prompt tokens NOT read from cache,
-- completion tokens, and prompt tokens read from cache. They are kept in memory on the proxy's hot
-- path and flushed here once per live-poll pass, like `last_output_at`.
--
-- `startup_script_id` is the Verda startup script created for this claim, set before the instance
-- is asked for and kept as history after the script is deleted; NULL on RunPod, which has no
-- startup script. `engine_version` is what the model server reported at readiness, "vllm
-- <version>" or "llama.cpp <build_info>", '' when it could not be read.
--
-- `ssh_host_key` is a Verda machine's SSH host key, in OpenSSH authorized_keys form, pinned the
-- first time this program's SSH tunnel connects to it (internal/sshtunnel); NULL until then and
-- always on RunPod, which has no tunnel. Public text, never a secret.
--
-- `launch_request` is the launch's structured request as JSON (console.launchRequest), kept so the
-- auto-move can rebuild a launch for a cheaper shape; '' when none was stored, and such a claim is
-- never moved. It holds no credential.
CREATE TABLE IF NOT EXISTS machine_claim (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_key       TEXT NOT NULL,
    shape_code         TEXT NOT NULL,
    location           TEXT NOT NULL,
    hostname           TEXT NOT NULL,
    price_per_hour     REAL,
    asked_at           TEXT NOT NULL,
    provider_id        TEXT,
    state              TEXT NOT NULL,
    detail             TEXT,
    settled_at         TEXT,
    project            TEXT NOT NULL DEFAULT 'coding',
    model_name         TEXT NOT NULL DEFAULT '',
    image_or_template  TEXT NOT NULL DEFAULT '',
    serve_args         TEXT NOT NULL DEFAULT '',
    serve_env_keys     TEXT NOT NULL DEFAULT '',
    serving_state      TEXT NOT NULL DEFAULT '',
    loading_since      TEXT,
    serving_since      TEXT,
    last_output_at     TEXT,
    tokens_in          INTEGER NOT NULL DEFAULT 0,
    tokens_out         INTEGER NOT NULL DEFAULT 0,
    tokens_cached      INTEGER NOT NULL DEFAULT 0,
    startup_script_id  TEXT,
    engine_version      TEXT NOT NULL DEFAULT '',
    ssh_host_key       TEXT,
    launch_request     TEXT NOT NULL DEFAULT '',
    idle_minutes       INTEGER NOT NULL DEFAULT 30
);

-- `model_setting` holds the user's saved default for one model, one row per key:
-- `context_thousands`, `chats`, the five sampling values (`temperature`, `top_k`, `top_p`,
-- `min_p`, `repeat_penalty`), `gpu_memory_utilization`, and `thinking` (1 on, 0 off). A missing
-- row means "not set", and `internal/console` fills the gap from the catalog's value for the
-- five sampling keys, or from the model server's default for the rest. This is never where
-- `catalog/models.json` is written back to: a row here is the user's choice, kept apart from the
-- committed catalog.
CREATE TABLE IF NOT EXISTS model_setting (
    model_name TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    PRIMARY KEY (model_name, key)
);

-- `destroy_intent` is the destroy record: written and committed the instant Delete is pressed,
-- before the provider is asked anything. A process that dies mid-destroy leaves this row saying
-- exactly what was being destroyed and when it was asked for. `gone_at` is written only once a
-- FRESH provider list no longer carries the machine; a null `gone_at` with an old `asked_at` is a
-- destroy that never got a fresh answer confirming it.
CREATE TABLE IF NOT EXISTS destroy_intent (
    provider_key    TEXT NOT NULL,
    provider_id     TEXT NOT NULL,
    display_name    TEXT NOT NULL,
    price_per_hour  REAL,
    asked_at        TEXT NOT NULL,
    gone_at         TEXT,
    PRIMARY KEY (provider_key, provider_id)
);

-- `machine_move` is the auto-move's record, written first and finished by the watcher after a
-- restart: one row per move of a project's serving model from an old machine to a cheaper new one,
-- committed after the new claim row and BEFORE the provider is asked to create the new machine, so
-- a process that dies mid-move leaves a row a restarted program finishes. `old_claim_id` and
-- `new_claim_id` are machine_claim ids. `state` is 'starting' (the new machine is being created or
-- loaded; the old one keeps serving), 'draining' (the new one serves and takes new requests; the
-- old one is destroyed once nothing is in flight on it), 'done' (the old machine is confirmed
-- gone) or 'failed' (the new one was refused or never served; the old one keeps serving). Never
-- deleted.
CREATE TABLE IF NOT EXISTS machine_move (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    project       TEXT NOT NULL,
    old_claim_id  INTEGER NOT NULL,
    new_claim_id  INTEGER NOT NULL,
    state         TEXT NOT NULL,
    started_at    TEXT NOT NULL,
    switched_at   TEXT,
    finished_at   TEXT,
    detail        TEXT
);
