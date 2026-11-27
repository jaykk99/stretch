"""Real synthetic causal-LM layer offload smoke test, no external model download."""
import tempfile
from pathlib import Path
from tokenizers import Tokenizer
from tokenizers.models import WordLevel
from tokenizers.pre_tokenizers import Whitespace
from transformers import GPT2Config, GPT2LMHeadModel, PreTrainedTokenizerFast
from stretch_stream import run

with tempfile.TemporaryDirectory() as d:
    root = Path(d)
    model = root / "model"
    model.mkdir()
    vocab = {"[UNK]":0,"[EOS]":1,"hello":2}
    vocab.update({f"word{i}":i for i in range(3,64)})
    tok = Tokenizer(WordLevel(vocab, unk_token="[UNK]"))
    tok.pre_tokenizer = Whitespace()
    PreTrainedTokenizerFast(tokenizer_object=tok, unk_token="[UNK]", eos_token="[EOS]").save_pretrained(model)
    GPT2LMHeadModel(GPT2Config(vocab_size=64,n_positions=64,n_embd=256,n_layer=8,n_head=4,
                    bos_token_id=1,eos_token_id=1)).save_pretrained(model, safe_serialization=True)
    status = run([str(model), "--offload-dir", str(root/"disk"), "--ram-mib", "8",
                  "--require-offload", "--prompt", "hello", "--context", "16", "--tokens", "2"])
    if status:
        raise SystemExit(status)
    print("Layer offload smoke test completed.")
