# Providers and model servers

What RunPod, Verda, vLLM, llama.cpp and Hugging Face actually do, as learned by running real machines on them, and what GPUGate does about each. These are facts about other people's systems, so each carries how we know it: `[DOC]` with the source, `[OBS]` with when it was seen, or `[UNKNOWN]`. Anything here can change on their side without notice.

## Contents

- [Both providers](#both-providers)
- [RunPod](#runpod)
- [Verda](#verda)
- [Model servers](#model-servers)
- [Multi-card machines](#multi-card-machines)
- [Hugging Face](#hugging-face)
- [Open questions](#open-questions)

## Both providers

- **Capacity changes within minutes.** One empty answer does not mean a provider offers nothing, and a machine shown as available can be gone a second later. `[OBS]` August to October 2026.
- **A provider refusing a create is a normal answer**, reported to you in the provider's own words. GPUGate never quietly tries a different machine instead.
- **Never install anything with `apt` on a fresh machine.** The boot-time package process holds the lock, and a bring-up that waited on it failed. `[OBS]` 2026-08.
- **Disk is sized from the model.** A 55 GB model needs about 30 GB of container disk and 120 GB of volume; the defaults some templates offer (5 and 25 GB) cannot hold one. `[OBS]` 2026-08.

## RunPod

- **Secure cloud only.** GPUGate never asks for community or spot machines.
- **A pod is the serving container.** There is no machine to log into: the image, the key and every serving flag go in when the pod is created, and nothing can be changed afterwards. `[OBS]` 2026-08.
- **Stock must be read per card count.** `GET /v2/catalog/gpus?include=AVAILABILITY&product=POD&cloud=SECURE&count=N` answers which data centres can supply N cards of each type. A read per card type alone makes one free card look like an orderable two, four or eight card machine, and every such create is refused. `[DOC]` docs.runpod.io/api-reference-v2/catalog/list-gpu-types, read 2026-10-12; `[OBS]` 2026-10-12: two-card creates were refused while stock was read per card type. Even the per-count answer is a snapshot, and whether "two cards" means two in one machine is `[UNKNOWN]`.
- **The vLLM template's own API key is derived from the public pod id**, so anyone who knows the pod's address can use it. GPUGate sets its own random key in the pod's environment at create. `[OBS]` 2026-08.
- **llama.cpp needs no template.** GGUF models run on the official llama.cpp server image, pinned to one tag, with its arguments given directly.
- **The pod's address is `https://<pod-id>-8000.proxy.runpod.net`**, behind Cloudflare. Two consequences:
  - Python's default User-Agent is refused with 403 ("error code: 1010"). `[OBS]` 2026-09-12. GPUGate sends one plain browser User-Agent on every request.
  - The proxy answers 524 when a pod sends nothing for about 100 to 120 seconds, which a long first token can trigger. `[OBS]` 2026-09-07.
- **`GET /v2/pods/{id}/logs` is a stream that never closes.** A plain read hangs forever; GPUGate reads it for a bounded time. `[OBS]` 2026-08.
- **A pooled HTTP/2 connection to RunPod can go silently dead** for minutes. GPUGate's HTTP client pings idle connections so a dead one is dropped. `[OBS]` 2026-09-13.
- **`DELETE /v2/pods/{id}`** removes the pod and its own storage; a 404 means it is already gone.
- **The REST API can be down for days**: pod listing failed on two consecutive days. `[OBS]` 2026-08-27 and 28.

## Verda

- **Credentials are OAuth client credentials.** `POST /v1/oauth2/token` gives a token valid for an hour. The token endpoint fails now and then (404, timeouts, 502 on different days), so a failed call is shown as "could not be asked", never as an empty account. `[OBS]` 2026-08 and 09. Rate limits are 500 requests a minute per project and 60 a minute per endpoint. `[DOC]` Verda API documentation, read 2026-08.
- **Only port 22 answers on a machine's public address, and Verda publishes no host key.** So GPUGate runs the model server bound to the machine's own loopback, reaches it through an SSH connection to that machine, and pins the host key it sees on first connect. Readiness checks, the machine's log and the fixed address all go through that connection. `[OBS]` 2026-09-21.
- **There is no log endpoint.** The model server's output is written to a file on the machine and read over SSH.
- **`ssh_key_ids` must be given on create**, with real key ids, although the API specification says it may be left out or null; both are refused. GPUGate reads the account's keys at launch. `[OBS]` 2026-09.
- **`location_code` is required on create.** `[DOC]` Verda release notes, change of 2026-03-23.
- **Read the machine image live.** `GET /v1/images?instance_type=<type>` lists the images a machine type can boot; GPUGate picks the newest Ubuntu 24.04 image with Docker and CUDA 13. Image names that were pinned have disappeared. `[OBS]` 2026-09-20.
- **Machine types ending in `.CC` are confidential computing** and offer no Docker image, so GPUGate cannot serve on them. The same card without `.CC` works. `[OBS]` 2026-09-23.
- **Startup scripts run under a start timeout** too short for pulling a model server image of several GB. The script only writes the key to a root-only file and hands the pull and the run to a separate systemd unit. `[DOC]` forum.verda.com/t/timed-out-start-script/38. GPUGate creates one startup script per launch and deletes it once the machine serves or is given up.
- **Create answers 202 with the new id**, sometimes bare, sometimes quoted. `[OBS]` 2026-09.
- **A destroyed machine still answers 200 on `GET /v1/instances/{id}`, with status `discontinued`.** Only the list drops it. GPUGate treats that status as gone. `[OBS]` 2026-09-21.
- **Destroy is `PUT /v1/instances` with `{"action": "delete", "id": ...}`**, and takes the machine's OS volume with it.
- **Read the error code, not the message.** Only `service_unavailable` and the instance status `no_capacity` mean there is no capacity; a deleted startup script id once came back looking like a capacity error. `[OBS]` 2026-08.

## Model servers

- **vLLM is the official `vllm/vllm-openai` image**, on both providers, unpinned, so a new vLLM release can change behaviour without any change here. llama.cpp is the official server image, pinned.
- **vLLM needs `--enable-auto-tool-choice`** with a tool-call parser, or a coding tool's first tool call is refused with 400. `[OBS]` 2026-09.
- **vLLM needs `--enable-prompt-tokens-details`**, or it never reports how many prompt tokens came from cache. `[DOC]` vLLM 0.29.0.
- **vLLM refuses to start** unless `--long-prefill-token-threshold` is at most the context and `--max-num-batched-tokens` is at least the number of chats; GPUGate derives both from the launch. `[DOC]` vLLM 0.27.1.
- **vLLM ignores a chat-template setting the template does not declare**, so GPUGate sends both `enable_thinking` and `thinking` and each model's template takes the one it knows. `[DOC]` vLLM source, `hf.py`.
- **A request names the model by its Hugging Face repository**, which is what vLLM serves it as; any other name is refused. `[OBS]` 2026-10-05.
- **The Qwen GGUF chat template refuses a system message anywhere but first** ("System message must be at the beginning"), and llama.cpp's Anthropic endpoint passes messages through as they come. GPUGate's fixed address moves system messages to the front for llama.cpp. `[OBS]` 2026-09-07.
- **llama.cpp's `-c` is the whole context pool shared by all chats**, so GPUGate sets it to the context times the number of chats, with one slot per chat. Its key and cache folder are environment variables, not flags. `[DOC]` llama.cpp server, tag b10818.
- **Sliding-window models cannot be sized from one key-value figure.** One such model needed 48 GiB of cache at 131,072 tokens where a single constant predicted 11, and vLLM refused to start it. `[OBS]` 2026-09.

## Multi-card machines

- **vLLM needs `--tensor-parallel-size` set to the card count**, or it loads onto one card and never serves. `[OBS]` 2026-08-29.
- **Two-card machines can hang after warm-up**, printing only "No available shared memory broadcast block found in 60 seconds" once a minute. The closest public report, vllm-project/vllm#45198, names `--disable-custom-all-reduce` as a workaround. GPUGate sends it, with `NCCL_P2P_DISABLE=1` and `NCCL_DEBUG=INFO`, on every multi-card machine. Two-card machines have both served and hung with these settings, so whether they cure the hang is `[UNKNOWN]`. A machine that shows only that message for ten minutes is destroyed. `[OBS]` 2026-10-11 and 12.
- **Two-card stalls can also stop right after NCCL starts**, on hosts without NVLink or direct card-to-card transfers; `NCCL_P2P_DISABLE=1` got past it. `[OBS]` 2026-08-31.

## Hugging Face

- **Downloads without a token are much slower.** The same model took 1,646 seconds to serve without a token and 646 with one. GPUGate always passes the token when one is set. `[OBS]` 2026-09-23.

## Open questions

- What `NCCL_P2P_DISABLE=1` costs in decode speed. `[UNKNOWN]`
- Whether RunPod's per-count stock means that many cards in one machine. `[UNKNOWN]`
- Whether Verda keeps its own copy of a startup script's body after the script is deleted. `[UNKNOWN]`
