# Adding a model to the catalog

How to add a model to `catalog/models.json`: what each field is, where its value comes from, how to check a new row, and what fails when a figure is wrong. Tags: `[DOC]` a source read, `[OBS]` seen on a date, `[UNKNOWN]` not established. How the fit uses these figures is in [the architecture](architecture.md); what the model servers do is in [providers](providers.md).

## Contents

- [What a catalog row is](#what-a-catalog-row-is)
- [The fields](#the-fields)
- [Computing the three size figures](#computing-the-three-size-figures)
- [Worked example: a safetensors row](#worked-example-a-safetensors-row)
- [Traps: sliding-window and hybrid models](#traps-sliding-window-and-hybrid-models)
- [GGUF rows versus safetensors rows](#gguf-rows-versus-safetensors-rows)
- [trust_remote_code](#trust_remote_code)
- [The parsers](#the-parsers)
- [Checking a new row before trusting it](#checking-a-new-row-before-trusting-it)
- [What goes wrong when a figure is wrong](#what-goes-wrong-when-a-figure-is-wrong)
- [What could not be established](#what-could-not-be-established)

## What a catalog row is

`catalog/models.json` is one JSON array. Each element is a row: one model, in one file format, at one quantisation. GPUGate offers exactly the rows in this file in its model dropdown, and uses a row for three things: the fit column (does this machine hold the model, at this context and chat count), the serving command line sent to the rented machine, and the refusals made before any money is spent.

- **When it is read.** Once, at program start (`catalog.Load` in `cmd/gpugate/main.go`, path `catalog/models.json` relative to the working directory). Editing the file changes nothing in a running program: restart it. `[OBS]` read in the code; nothing re-reads the file later.
- **Load failures.** A file that is not valid JSON, or has a value of the wrong type (a string where a number belongs), stops the program at start with "load the model catalog". `[OBS]` `internal/catalog/catalog.go` `Load` returns the `json.Unmarshal` error and `main.go` returns it.
- **There is no other validation.** Nothing checks that a name is unique, that a required field is present, that a figure is plausible, or that a key is spelled right: a column the Go code does not know is ignored without a word. A misspelled `kv_cache_bytes_per_token` is therefore read as absent, and the row becomes "cannot be sized". A misspelled `trust_remote_code` is read as false. Re-read the row after typing it.
- **One row is one file.** The same model in another quantisation is a second row naming the other file, never an edit to the first.
- **The name is the row's identity inside GPUGate.** Saved per-model settings are keyed by `name` (table `model_setting`), so renaming a row orphans what was saved for it. If two rows share a name, the later one silently wins in the program's lookup while the dropdown shows both.
- **What clients call it.** The model server serves the model under its `hf_repo` string, and a request must use that string. The catalog `name` is only what the dropdown shows. `[OBS]` `docs/providers.md` 2026-10-05; llama.cpp is started with `--alias <hf_repo>` so both engines agree (`internal/provider/runpod/runpod.go`).
- **Write down where every figure came from.** The shipped rows carry the basis of each figure in `architecture_basis` or `notes`, so the next person can repeat the arithmetic. The code does not require it.

## The fields

"Read by Go" means `internal/catalog/catalog.go` has a field for it. "Reference only" columns are kept so the next person can see where a figure came from; Go ignores them.

| Field | Required | Read by Go | What it is and where its value comes from |
| --- | --- | --- | --- |
| `name` | yes | yes | Dropdown label and key for saved settings. Free text; any unique string. |
| `hf_repo` | yes | yes | The Hugging Face repository the machine downloads, `owner/name`. Also the name the model is served and requested under. |
| `hf_file` | GGUF: yes. Otherwise `null` | yes | The exact `.gguf` file inside `hf_repo`, from the repository's Files tab or its tree listing. A GGUF launch is refused ("has no hf_file in the catalog") if it is empty. |
| `format` | yes | yes | `"gguf"`, `"safetensors"` or `"fp8"`. Decides the engine and which provider may serve it (see [GGUF rows](#gguf-rows-versus-safetensors-rows)). Any other value is refused by the provider's format check. |
| `size_gb` | yes | yes | Weight size in decimal gigabytes (bytes divided by 1,000,000,000). Used only when `disk_bytes` is null. |
| `disk_bytes` | strongly advised | yes | Exact byte total of the files the machine downloads. Wins over `size_gb` for the fit, and sizes the rented machine's disks. Null falls back to `size_gb` for the fit and to 30 GB container and 60 GB volume floors for disks. |
| `kv_cache_bytes_per_token` | yes in practice | yes | Memory one token of one conversation takes in the cache. Null makes the row "cannot be sized": it cannot be launched from the fit column. See [computing](#computing-the-three-size-figures). |
| `attention_layers`, `kv_heads`, `head_dim` | reference | no | The factors the cache figure was built from. |
| `state_layers` | `0` if none | yes | Number of layers that keep a recurrent state (hybrid models). Greater than zero marks the row "hybrid" in the dropdown. |
| `state_bytes_per_layer` | reference | no | Size of that state for one layer. |
| `weight_bits` | reference | no | Average bits per weight: file bytes times 8 over the parameter count. Sanity check for quantised files. |
| `quant_method` | reference | no | For example `compressed-tensors`; `null` when none. |
| `min_compute_capability` | optional | yes | The lowest CUDA compute capability the build runs on, as a two-digit integer (8.9 is `89`). Null never excludes a card. `[OBS]` A former FP8 row used 89 because the serving image refused FP8 activations on an A100 (compute capability 8.0). |
| `context_length` | optional | yes | The model's published maximum context in raw tokens. A launch asking for more is refused before anything is rented. Also shown as the "model" value beside the context box. Null means no ceiling is enforced. |
| `context_size` | reference | no | A second context figure carried by some rows. What it records is `[UNKNOWN]`: no code or document reads it. |
| `reasoning_parser`, `tool_call_parser` | optional | yes | vLLM flag values; see [the parsers](#the-parsers). Ignored for GGUF rows. |
| `temperature`, `top_k`, `top_p`, `min_p`, `repeat_penalty` | optional | yes | The publisher's recommended sampling values. They fill the "Model settings" boxes as the fallback layer: a value the user saved wins, and an emptied box sends nothing. Null means the row has none. |
| `trust_remote_code` | optional, default false | yes | See [trust_remote_code](#trust_remote_code). |
| `architecture_basis`, `notes` | advised | no | Arrays of strings, each tagged `[DOC]`, `[OBS]` or `[UNKNOWN]`, giving the source of every figure above. |

Where the sampling values come from: the repository's `generation_config.json` (keys `temperature`, `top_k`, `top_p`, `repetition_penalty`) or, when it has none, the base model's. The shipped GGUF row says exactly that in its notes. `[OBS]` shipped rows.

## Computing the three size figures

All three come from files you can fetch without a login for a public repository. Use the repository the row names (or, for a GGUF repository that carries no `config.json`, the base model's).

### `disk_bytes` and `size_gb`

- Safetensors: add up the byte sizes of every file the server downloads, which is the weight shards (`model-0000N-of-0000M.safetensors`). Hugging Face's model API with `?blobs=true` lists each file with its size. `[OBS]` the shipped Ornith row cites `https://huggingface.co/api/models/<repo>?blobs=true`, 16 shards summing to 71,903,871,064 bytes, seen 2026-09-10. Its `size_gb` is that number divided by 10^9, 71.9.
- GGUF: the byte size of the one file named by `hf_file`, from the repository's tree listing. The shipped Qwen row: 29,047,086,048 bytes, `size_gb` 29.05.
- Decimal GB, not GiB. GPUGate converts with a thousand million for `size_gb` and with 2^30 when it works in GiB internally. Typing a GiB figure into `size_gb` understates the model by about 7%.
- `disk_bytes` also sizes the rented machine's disks: container disk 30% of the weights (floor 30 GB), persistent volume 2.2 times the weights (floor 60 GB) (`internal/provider/disksize.go`). Wrong low and the download does not fit.

### `kv_cache_bytes_per_token`

For a model where every cached layer is ordinary full attention:

```
kv_cache_bytes_per_token = 2 x full_attention_layers x num_key_value_heads x head_dim x bytes_per_element
```

- The 2 is one key and one value.
- `bytes_per_element` is 2 for the default 16-bit cache. `[UNKNOWN]` for an 8-bit cache: GPUGate never sends a cache-dtype flag (`internal/provider/vllm.go`), so 2 is what the server uses on `--dtype auto`.
- Take the keys from the model's `config.json`. Multimodal models nest the language model's keys under `text_config`; read them there, not at the top level.
- `full_attention_layers` is the count of `"full_attention"` entries in `layer_types` when that list exists, otherwise `num_hidden_layers`. Layers listed as `"linear_attention"` or `"sliding_attention"` are not in this count (see the traps below).
- Some models name a different head count or head size for their full-attention layers (Gemma 4 uses `num_global_key_value_heads` and `global_head_dim`). Read the vendor's modelling code for which pair a full-attention layer uses; reading the plain `head_dim` was the cause of one wrong shipped figure. `[OBS]` git history of this catalog, 2026-09-12.

What the fit does with it (`internal/fit/fit.go`): a conversation costs `chats x context_tokens x kv_cache_bytes_per_token`. Whole-machine need is `weights + 1.3 x (2 GiB + 1 GiB per card + conversations)`, compared with 90% of the machine's total memory (every card summed). The 1.3, the 2, the 1 and the 0.9 are judgements, not measurements, named as such in the code.

## Worked example: a safetensors row

The shipped row `Ornith-1.5-35B-A3B` (format `safetensors`, repository `ornith-ai/Ornith-1.5-35B-A3B`). `[DOC]` its `config.json`, fetched again 2026-10-19 from `https://huggingface.co/ornith-ai/Ornith-1.5-35B-A3B/raw/main/config.json`, and it matches what the row stored on 2026-09-10.

Keys under `text_config`: `num_hidden_layers` 40; `layer_types` with a `full_attention` at every fourth position and `linear_attention` at the rest (10 and 30); `head_dim` 256; `num_key_value_heads` 2; `max_position_embeddings` 262144.

1. Cache per token: 2 x 10 x 2 x 256 x 2 = **20,480** bytes. This is the stored `kv_cache_bytes_per_token`. Check: at 262,144 tokens one conversation takes 20,480 x 262,144 = 5.0 GiB.
2. The other 30 layers are linear attention, a recurrent state, not a per-token cache. `state_layers` = 30.
3. State of one such layer: with `linear_key_head_dim` 128, `linear_num_key_heads` 16, `linear_value_head_dim` 128, `linear_num_value_heads` 32, `linear_conv_kernel_dim` 4, `dtype` bfloat16 (2 bytes) and `mamba_ssm_dtype` float32 (4 bytes):
   - convolution state: (128 x 16 x 2 + 128 x 32) x (4 - 1) x 2 = 49,152 bytes
   - temporal state: 32 x 128 x 128 x 4 = 2,097,152 bytes
   - together 2,146,304, the stored `state_bytes_per_layer`. `state_layers` x that is 61.4 MiB per conversation. GPUGate does not count it (see below); it is small here.
4. `disk_bytes`: sum of the 16 shards, 71,903,871,064; `size_gb` 71.9; `weight_bits` 16.0 for a bfloat16 repository.
5. `context_length`: `max_position_embeddings` = 262,144.
6. Sampling: `generation_config.json`: temperature 1.0, top_p 0.95, top_k 20.
7. Parsers: `qwen3_coder` and `qwen3`, the vLLM names for the Qwen3 family (see [the parsers](#the-parsers)).

The second shipped row, `Qwen3.8-27B-Q8_0`, reproduces the same way: 2 x 16 x 4 x 256 x 2 = **65,536**. Its repository is GGUF; the numbers come from the base model's `config.json`, which that repository keeps beside the files. Its state per layer is 61,440 + 3,145,728 = 3,207,168 bytes, 48 layers. `[OBS]` the row's own basis; the GGUF file itself was never opened to confirm.

## Traps: sliding-window and hybrid models

Three kinds of layer appear in modern `layer_types` lists. Count them separately.

- **Full attention.** Costs the per-token figure above, for the whole context.
- **Linear attention / state-space ("hybrid" models).** Holds a fixed-size state per conversation, not per token. The row records `state_layers` and `state_bytes_per_layer` and leaves them out of `kv_cache_bytes_per_token`. GPUGate's fit does not add that state, so its answer is slightly optimistic for such a row, roughly 0.4 to 1.5 GiB at ten chats (comment on `StateLayers` in `internal/catalog/catalog.go`). The dropdown labels such a row "hybrid state, sum is optimistic". Setting `state_layers` above zero is how a row says so; nothing else reads it.
- **Sliding-window attention.** A single constant per token cannot describe it honestly, and counting only the full-attention layers is wrong. `[OBS]` 2026-09-12, Gemma-4-31B-it on one 96 GB card at a context of 131,072 tokens: the row stored 81,920 bytes per token (10 full layers only), vLLM needed 48.29 GiB of cache against 26.3 available and refused to start 26 times in a loop (vLLM v0.29.0, the `vllm/vllm-openai:latest` image that day). `[DOC]` vLLM v0.29.0 sizes each sliding layer at `min(sliding_window - 1 + max_concurrent_batches x max_num_batched_tokens, max_model_len)` tokens (`vllm/v1/kv_cache_interface.py` lines 695 to 735), with `max_concurrent_batches` 2 because async scheduling is on by default (`vllm/config/vllm.py`), and each sliding layer uses its own kv-head count and head size, which can differ from the full layers'. That reproduced the pod's 48.29 GiB exactly. The row for such a model should therefore be derived as: full layers at the full context, plus each sliding layer at the capped length, divided by the context, and its notes must state the context, chat count and batched-token figure it was derived at, because the answer moves with them. `[UNKNOWN]` the formula was checked against one pod log and one other model's arithmetic, not against other vLLM versions.
- **Shared cache layers.** If the config says some layers reuse an earlier layer's cache, they are not counted (the shipped bases record "the last 0 sharing an earlier layer's").
- **Vision towers and multi-token-prediction heads** are not in any figure. Ornith's config lists `mtp_num_hidden_layers` 1; whether vLLM allocates anything for it at serving time is `[UNKNOWN]`.

## GGUF rows versus safetensors rows

| | safetensors / fp8 | gguf |
| --- | --- | --- |
| Engine | vLLM, official `vllm/vllm-openai` image | llama.cpp server, official image pinned to one tag |
| Providers | RunPod and Verda | RunPod only. Verda's format check refuses GGUF by name. |
| Weights | whole repository downloaded by vLLM | `-hf <hf_repo> -hff <hf_file>`: only the one named file |
| `hf_file` | `null` | required |
| Parsers, GPU-memory-utilization, batched-token flags | sent when set | never read; llama.cpp has no such flags |
| `trust_remote_code` | sent as `--trust-remote-code` | never reaches the command line |
| Sampling | one `--override-generation-config` JSON | bare `--temp --top-k --top-p --min-p --repeat-penalty` |

- GGUF support in vLLM is experimental and not installed in the official image, and GPUGate never rebuilds an image, so there is no vLLM path for a GGUF row. `[DOC]` vLLM v0.28.0 `docs/features/quantization/gguf.md`, quoted in the code comment on `ServesFormat` in `internal/provider/runpod/runpod.go`.
- llama.cpp loads the whole context pool shared by all chats: GPUGate sets `-c` to context times chats. `[DOC]` llama.cpp server README at tag b10818 (the pinned tag at the time of that comment; re-read before relying on it).
- llama.cpp does not read the model's `generation_config.json`; its own defaults apply where a row carries no value (temperature 0.80, top-k 40, top-p 0.95, per the program's own settings text). `[OBS]` `engineDefaultText` in `internal/console/model_settings.go`.
- For a GGUF row the architecture figures come from the base model's `config.json`, not from the file. The file's own header (`gguf_dump`) or the server's startup log is the proof, and the shipped row marks that as `[UNKNOWN]`.
- A GGUF repository often publishes dozens of quantisations and vision-projector files (`mmproj-*.gguf`). The row names one weights file; GPUGate downloads only that file, so a row serves text, not images.
- `quant_method` and `weight_bits` are reference columns; `weight_bits` = file bytes x 8 / parameter count (the shipped row: 8.51).
- `format: "fp8"` is accepted and goes to vLLM, but no FP8 row ships today. FP8 weights need compute capability 8.9 or higher; a build that also quantises activations refused to load on an 8.0 card even though the weights fitted. `[OBS]` removed row note, git history of this catalog. Set `min_compute_capability` accordingly.

## trust_remote_code

`"trust_remote_code": true` makes GPUGate add `--trust-remote-code` to the vLLM command. That flag lets vLLM run Python code that lives in the model's own repository (custom model classes listed under `auto_map` in its `config.json`). The rented machine then runs code you have not reviewed unless you read it. What that exposes is bounded by what the machine holds: its own random serving key and, when configured, the Hugging Face token. It holds no cloud-account credential. `[OBS]` `provider.VLLMServeEnv` in `internal/provider/vllm.go`. The launch panel adds a sentence saying so when the chosen row sets it.

- Leave it out unless the model fails without it. Most rows do not need it: the model's class is already in vLLM's registry. How to tell: the repository has no `.py` files and no `auto_map` in `config.json` (checked through the Hugging Face model API), and the `architectures` entry is registered in vLLM's `vllm/model_executor/models/registry.py`. The shipped Ornith row cites registry line 593 for v0.28.0.
- A model that needs it can still fail. `[OBS]` 2026-09-21 a model carrying the flag crashed inside its own repository code on vLLM 0.29.0 on every load and its row was dropped.
- The flag never applies to GGUF rows.

## The parsers

`tool_call_parser` and `reasoning_parser` are values for vLLM's `--tool-call-parser` and `--reasoning-parser`. Both are optional; empty means the flag is not sent.

- A tool-call parser alone is not enough: GPUGate sends `--enable-auto-tool-choice` with it, because without that flag vLLM answers a coding tool's `tool_choice: auto` request with 400. `[OBS]` 2026-09. A row with no tool-call parser sends neither flag, so such a model will refuse a coding tool's tool calls.
- Reasoning parser: splits the model's thinking from its answer into a `reasoning` field. `[DOC]` vLLM v0.28.0 `docs/features/reasoning_outputs.md`.
- Where to find the right value: the model card, or vLLM's docs on tool calling and reasoning outputs, which list the parser names and which model families each belongs to. The shipped values are `qwen3_coder` with `qwen3` (Qwen3 family) and, from a former row, `gemma4` for both. A parser copied from a sibling model of the same family is a reasonable first guess and is an assumption: the shipped Ornith row says so in its notes.
- GPUGate always sends `--default-chat-template-kwargs` with both `enable_thinking` and `thinking`; the model's chat template uses whichever it declares. `[DOC]` vLLM `hf.py`.
- A parser name vLLM does not know: expected to stop vLLM at start-up. The exact message was not observed. `[UNKNOWN]`

## Checking a new row before trusting it

Nothing tests a new row automatically, and a first launch is the only complete proof, because it is the only step that loads the model on a card. A launch costs money. Everything below is free.

1. **Restart** the program and watch the start: a parse error or a wrong type stops it, naming the file.
2. **Open the dropdown.** The row appears as `name [format]`, with "hybrid state, sum is optimistic" if `state_layers` is above zero. A row missing from the list was not read.
3. **Choose it and open the machine table.** The fit column shows one verdict per machine: launchable (with the free GiB), too small (short by N GiB), cannot be sized (no `kv_cache_bytes_per_token`), card cannot run it (below `min_compute_capability`), cannot serve this format (a GGUF row on Verda). If everything says "cannot be sized", the cache figure did not load.
4. **Compare the verdict with your own arithmetic.** For a chosen context and chat count, work out `weights + 1.3 x (2 + cards + chats x context x kv bytes / 2^30)` GiB and compare with 0.9 times the machine's memory. The console's number should match yours. A large gap means a figure is mis-typed.
5. **Check the context ceiling.** A context above `context_length` is refused with the model's published maximum named in the message.
6. **Re-read the sources.** The row's notes should let a stranger repeat the arithmetic; if you cannot reproduce `kv_cache_bytes_per_token` from `config.json`, the row is not ready.
7. **Then the real launch.** It is the only check that the repository downloads, the architecture is supported by the pinned or latest server, the parsers are valid, the cache really fits, and the model answers. Do it on the smallest machine the fit column calls launchable, at a short context, and read the machine log if it does not serve. GPUGate gives a launch one hour to serve and destroys the machine after that.

## What goes wrong when a figure is wrong

| Wrong figure | What you see |
| --- | --- |
| `kv_cache_bytes_per_token` too low (a sliding-window model counted by its full layers only) | The fit column says launchable. The machine rents, then vLLM refuses to start with "KV cache is needed, which is larger than the available KV cache memory" and restarts in a loop until the hour clock ends and GPUGate destroys it. The log names the cache size actually needed, which is the number to put back in the row: `needed GiB x 2^30 / context`. `[OBS]` 2026-09-12. |
| `kv_cache_bytes_per_token` too high | The fit column says too small for machines that would work. Nothing fails; capacity is wasted. |
| `kv_cache_bytes_per_token` null or misspelled | "cannot be sized" on every machine. |
| `size_gb` or `disk_bytes` too low | Optimistic fit; the machine's disks may be too small for the download. `[UNKNOWN]` the exact message. |
| `hf_file` empty or wrong on a GGUF row | Empty: refused at launch before renting, "has no hf_file in the catalog". A name that does not exist in the repository: not checked by GPUGate; llama.cpp fails on the machine. `[UNKNOWN]` the exact message. |
| `hf_repo` wrong | The download fails on the machine and it never serves; the one-hour clock destroys it. Also the name clients must use is wrong. |
| `format` wrong | A GGUF repository labelled `safetensors` goes to vLLM and cannot load. A safetensors repository labelled `gguf` is refused on Verda and sent to llama.cpp on RunPod. |
| `context_length` too low | Needlessly refuses launches. Too high or null: the launch passes the check and vLLM refuses at start-up. |
| `state_layers` left at 0 on a hybrid model | No warning label; memory slightly under-counted. |
| `min_compute_capability` too high | Cards that could run it are marked "card cannot run it". Too low or null: a card that cannot run it is let through and the load fails on the machine. |
| A parser value vLLM does not know | `[UNKNOWN]` exact failure; expected at vLLM start-up. A missing tool-call parser: tool calls refused with 400. |
| Spelling of a key | Silently ignored (see above). |

A failed launch is the expensive way to learn any of these; the notes in the shipped rows exist because one such launch had already taught it.

## What could not be established

- What `context_size` records. No code or document says; it is `262144` on one shipped row and `null` on the other.
- Whether the cache dtype is always 16 bits for a model GPUGate serves (no cache-dtype flag is sent; vLLM's default behaviour under `--dtype auto` for a quantised-KV model was not read).
- The exact error text for a wrong `hf_file`, a wrong parser name, or too-small disks. None was observed.
- Whether the sliding-window formula holds for vLLM versions other than 0.29.0. The vLLM image is not pinned (`vllm/vllm-openai`, unpinned), so a later release may change the numbers.
- Whether vLLM allocates anything for a multi-token-prediction head or for the vision tower of a multimodal row. The fit ignores both.
- How a gated Hugging Face repository behaves: GPUGate passes the token when one is set but has no gated-model handling; whether a licence-gated model can be launched has not been tested. The shipped rows are public (`gated=false`).
- The vLLM and llama.cpp flag behaviour above rests on the vendor pages named beside each claim, read at the versions named; re-read them before relying on a newer version. Ornith's `config.json` was re-fetched on 2026-10-19 and matches its row.
