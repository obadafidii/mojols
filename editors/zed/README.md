# mojols for Zed

The mojols language server for Mojo, in Zed.

This extension is only the language server. Syntax highlighting, the grammar
and snippets come from the [Mojo extension](https://github.com/vadim-su/zed_mojo)
(`mojo`), so install both.

## Setup

1. Install the `mojo` and `mojols` extensions.
2. The `mojo` extension also starts the official `mojo-lsp-server`. mojols
   already runs and supervises that server, so turn the direct one off in your
   settings:

   ```json
   "languages": {
     "Mojo": { "language_servers": ["mojols", "!mojo-lsp-server", "..."] }
   }
   ```

3. Open a Mojo project. `mojols doctor`, run in the project folder, shows which
   `mojo-lsp-server` mojols found (a pixi, magic or venv environment in the
   project first, then PATH).

## Finding the mojols binary

In this order: `lsp.mojols.binary.path` in settings, `mojols` on the worktree's
PATH, a copy the extension downloaded before, then the latest
[GitHub release](https://github.com/obadafidii/mojols/releases)
(`mojols_<os>_<arch>.tar.gz`).

Extra arguments and environment go in the same settings block:

```json
"lsp": {
  "mojols": {
    "binary": {
      "path": "/path/to/mojols",
      "arguments": ["--log-file", "/tmp/mojols.log"]
    }
  }
}
```

`mojols serve --help` lists the flags.

## Developing

`zed: install dev extension` and pick this folder. `make zed-check` from the
repository root type-checks it.
