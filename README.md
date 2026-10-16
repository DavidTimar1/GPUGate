# GPUGate

**Rent a GPU machine on RunPod or Verda that is already serving the model you picked, point your coding tool at one fixed local address, and have the machine destroyed when you stop using it.**

> [!WARNING]
> **This project is not maintained.** It was built as a hobby and is published as is, in case it is useful to someone. There are no releases, no support, and no one reads issues or pull requests. It rents machines that cost real money on your own provider accounts: read the code before you trust it, and fork it freely.

## Contents

- [What it does](#what-it-does)
- [Requirements](#requirements)
- [Build](#build)
- [Running it](#running-it)
- [The Verda SSH key](#the-verda-ssh-key)
- [Security at a glance](#security-at-a-glance)
- [Documentation](#documentation)
- [Licence](#licence)

## What it does

- Lists every GPU machine RunPod and Verda will rent right now, with live prices and the locations that can supply it.
- Says, for the model you pick from its catalog, at the context window and number of parallel chats you want, which machines it fits on and how much room is left.
- Rents one with a click. The machine boots straight into serving the model with vLLM (or llama.cpp for GGUF models on RunPod), with its own random API key.
- Gives each project a fixed local address, `http://<host>:8719/p/<project>/v1`, that speaks the OpenAI and Anthropic APIs and forwards to whichever machine serves that project. Your tools never see the machine or its key.
- Destroys a machine when you press Delete, when it has been idle for its time limit, or when it fails to serve within an hour.
- Optionally moves a project to a cheaper machine on its own, among the machines you allowed, and switches the address over with no gap.
- Counts tokens and cost per rental.

## Requirements

- Go, at the version `go.mod` names. It is the only build tool: the console page is committed already built.
- Node, only if you change the page in `ui/src` and need to rebuild it. Nothing is installed with it.
- A RunPod account with an API key, a Verda account with a client id and secret, or both.
- Optionally a Hugging Face token, so a machine's model download is authenticated instead of rate limited.

## Build

```
go build -o dist/gpugate ./cmd/gpugate
```

`./gate.sh build` does the same. After a change in `ui/src`, run `npm --prefix ui run build` to copy it into `ui/dist`, and commit both.

## Running it

Copy [`example.env`](example.env) to `.env` in the project folder and fill in what you use. It lists every setting the program reads, each with a comment. A variable already set in the environment wins over `.env`.

Then start it:

```
./gate.sh start
```

The other commands are `status`, `stop`, `restart`, `log` and `run`:

- `start` builds into `dist/`, starts the program in the background and prints its addresses.
- `status` says whether it is running and where.
- `stop` stops it, and `restart` stops it and starts it again.
- `log` follows its output.
- `run` builds and runs it in the foreground.

Open `/console` on one of the printed addresses. By default GPUGate listens on `127.0.0.1:8719` only. `GPUGATE_LISTEN` (a comma-separated list of IP addresses) and `GPUGATE_PORT` change that, and the program binds exactly the addresses named, never every interface.

The console is behind one password. Until you set one, the console answers nothing but a login page that says how. Set it, or replace it, from the project folder:

```
./dist/gpugate set-password
```

It asks twice and shows nothing, works while the program runs, and logs every browser out. To make an access key for tools, run:

```
./dist/gpugate make-key
```

It prints a random key to put in `GPUGATE_ACCESS_KEY`. How the login, the sessions and the access key work is in [the architecture](docs/architecture.md#the-login).

These commands work without the console, run from the project folder after `./gate.sh build`:

```
./dist/gpugate list
./dist/gpugate destroy <provider> <id>
```

`list` prints every machine on both accounts, or why an account could not be asked. `destroy` destroys one machine and waits for a fresh list to confirm it gone.

Under Projects in the console each project has its own price cap in dollars per hour. Launch and the auto-move for that project refuse any machine priced over it or with no price. Empty means no cap.

## The Verda SSH key

Only SSH answers on a Verda machine's public address, so GPUGate reaches the model server through an SSH connection and needs a private key to sign it with. The key lives at `.ssh/verda/private` in the project folder, with no passphrase and mode 0600. Make one and register its public half on your Verda account:

```
mkdir -p .ssh/verda
ssh-keygen -t ed25519 -N "" -f .ssh/verda/private
chmod 600 .ssh/verda/private
cat .ssh/verda/private.pub
```

Add the printed public key in the Verda console under Keys, SSH Keys, Create (`[DOC]` docs.verda.com, "Managing SSH Keys", read 2026-10-13). Verda's guide recommends a passphrase for a personal key; this one has none because GPUGate cannot ask for one, so keep the file private. A Verda launch is refused, in plain words, when the key file is missing or when its public half is not one of the account's keys. RunPod needs no key. The `.ssh/` folder is ignored by git, so the private key is never committed.

## Security at a glance

| Concern | How GPUGate handles it |
|---|---|
| Console | One password, set from the command line with `./dist/gpugate set-password` and stored as an Argon2id hash. No second factor. Sessions end when the program restarts or the password changes. |
| Guessing | Repeated wrong passwords or access keys lock the address they came from for a short time, and a flood from anywhere locks login for everyone. |
| Tools | The fixed address `/p/<project>` and `GET /api/projects` take an access key you set in `GPUGATE_ACCESS_KEY`. Without one they are open to anyone who can reach a listening address. |
| Network | Plain HTTP on the addresses you name, loopback by default. Keep any other address to a Tailscale network: the program warns at start about the rest. |
| Credentials | Read from the environment or a `.env` file, never logged and never stored in the database. A machine receives only its own random key and, if you set one, the Hugging Face token for its model download. |
| Machines | The program rents and destroys machines on your accounts, which costs real money. Read the code before you trust it. |

The limits (password length, session life, lockout counts, key length) and the rest of the login are in [the architecture](docs/architecture.md#the-login).

## Documentation

- [Architecture](docs/architecture.md): how the program is put together and why.
- [Providers and model servers](docs/providers.md): what RunPod, Verda, vLLM and llama.cpp actually do, learned from real machines.

The code and its comments are the detailed documentation.

## Licence

GNU Affero General Public License v3.0 or later. See [LICENSE](LICENSE). The third-party code and fonts the project includes are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). The project is not maintained: see [CONTRIBUTING.md](CONTRIBUTING.md).
