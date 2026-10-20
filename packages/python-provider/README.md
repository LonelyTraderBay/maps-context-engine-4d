# Python provider

This package analyzes captured Python source with CPython's `ast` parser. It does not import target modules, execute target code, run project scripts or plugins, access the network, inspect notebook cells, or modify captured source.

The provider supports `.py` and `.pyi` declarations, lexical scopes, static imports, and bounded uncertainty diagnostics. Typed enrichment is reported as unavailable until a separately verified adapter is added. Static imports resolve only when their module source is present in the captured file set; the provider never searches the target filesystem.

The package uses only the Python standard library. The local profile pins CPython implementation/version, executable digest, provider digest, captured module roots, and analysis mode. The host supplies `profile_id`, which is the canonical effective-profile identity.

Run from the repository root:

```powershell
$env:PYTHONPATH = "packages/python-provider/src"
python -B -m unittest discover -s packages/python-provider/test -v
```

Run the process provider with the same `PYTHONPATH` and `python -B -m mce4d_python_provider.cli`. Standard output contains one Content-Length framed response only; failures return a short generic message on standard error.
