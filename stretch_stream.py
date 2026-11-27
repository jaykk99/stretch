#!/usr/bin/env python3
"""Experimental layer-by-layer disk offload for local Transformers safetensors."""
import argparse
import json
from pathlib import Path
import shutil
import sys
import tempfile
from stretch_model import available_ram, MIB

def run(argv=None):
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("model", type=Path, help="local Transformers snapshot with safetensors")
    p.add_argument("--offload-dir", type=Path, required=True, help="real disk or SSD directory")
    p.add_argument("--ram-mib", type=int, help="maximum CPU weight budget, not a total RSS cap")
    p.add_argument("--dtype", choices=["float32", "bfloat16"], default="float32")
    p.add_argument("--prompt", default="Hello")
    p.add_argument("--tokens", type=int, default=16)
    p.add_argument("--context", type=int, default=256)
    p.add_argument("--plan", action="store_true")
    p.add_argument("--require-offload", action="store_true")
    a = p.parse_args(argv)
    if a.tokens < 1 or a.context <= a.tokens or (a.ram_mib is not None and a.ram_mib < 1):
        p.error("positive RAM and tokens required; context must exceed tokens")
    model_dir = a.model.expanduser().resolve()
    offload_dir = a.offload_dir.expanduser().resolve()
    if not (model_dir / "config.json").is_file() or not list(model_dir.glob("*.safetensors")):
        p.error("provide a local safetensors model directory; pickle checkpoints are not accepted")
    try:
        import torch
        from accelerate import init_empty_weights, infer_auto_device_map
        from transformers import AutoConfig, AutoModelForCausalLM, AutoTokenizer
    except ImportError as e:
        p.error("Install torch, transformers, accelerate and safetensors in a virtualenv: " + str(e))
    ram = available_ram()
    if ram is None and a.ram_mib is None:
        p.error("RAM detection unavailable; specify --ram-mib explicitly")
    budget = max(0, int(ram*.65)-512*MIB) if ram is not None else a.ram_mib*MIB
    if a.ram_mib is not None:
        budget = min(budget, a.ram_mib*MIB)
    if budget < MIB:
        p.error("insufficient memory budget for CPU execution")
    dtype = getattr(torch, a.dtype)
    config = AutoConfig.from_pretrained(model_dir, local_files_only=True, trust_remote_code=False)
    with init_empty_weights():
        empty = AutoModelForCausalLM.from_config(config, trust_remote_code=False, torch_dtype=dtype)
    empty.tie_weights()
    device_map = infer_auto_device_map(empty, max_memory={"cpu": budget},
            no_split_module_classes=getattr(empty, "_no_split_modules", None) or [],
            dtype=dtype, offload_buffers=True)
    del empty
    destinations = set(device_map.values())
    print(json.dumps({"cpu_weight_budget_mib": budget//MIB, "dtype": a.dtype,
                      "device_map": device_map, "disk_offload": "disk" in destinations}), flush=True)
    if "disk" in destinations and "cpu" not in destinations:
        p.error("all layers mapped to disk; budget cannot hold a CPU layer plus working space")
    if a.require_offload and "disk" not in destinations:
        p.error("no disk layers selected; lower CPU weight budget")
    if a.plan:
        return 0
    offload_dir.mkdir(parents=True, exist_ok=True)
    # Disk staging may expand lower-precision checkpoint weights to float32.
    source_bytes = sum(f.stat().st_size for f in model_dir.glob("*.safetensors"))
    if "disk" in destinations and shutil.disk_usage(offload_dir).free < 4*source_bytes + 256*MIB:
        p.error("insufficient disk staging space; choose a larger disk or lower precision")
    tokenizer = AutoTokenizer.from_pretrained(model_dir, local_files_only=True, trust_remote_code=False)
    inputs = tokenizer(a.prompt, return_tensors="pt", truncation=True, max_length=a.context-a.tokens)
    with tempfile.TemporaryDirectory(prefix="stretch-layers-", dir=offload_dir) as folder:
        model = AutoModelForCausalLM.from_pretrained(
            model_dir, local_files_only=True, trust_remote_code=False, use_safetensors=True,
            torch_dtype=dtype, device_map=device_map, offload_folder=folder,
            offload_state_dict=True, offload_buffers=True, low_cpu_mem_usage=True)
        model.eval()
        # Accelerate hooks bring disk-resident weights onto CPU for forward passes.
        # Disable persistent KV caching to minimize memory; slower autoregression.
        with torch.inference_mode():
            output = model.generate(**inputs, max_new_tokens=a.tokens, do_sample=False,
                                    use_cache=False, pad_token_id=tokenizer.eos_token_id)
        print(tokenizer.decode(output[0][inputs["input_ids"].shape[1]:], skip_special_tokens=True))
        del model
    return 0

if __name__ == "__main__":
    sys.exit(run())
