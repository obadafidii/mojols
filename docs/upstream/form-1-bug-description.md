### Actual behavior

`mojo-lsp-server` aborts when it receives `textDocument/completion` in a file that imports anything from `std.iter`, with the cursor on the line after the import (or at the end of the import line):

```
LLVM ERROR: unknown decl symbol: @std::@builtin::@value::@RegisterPassable   (1.1.0)
LLVM ERROR: unknown decl symbol: @std::@traits::@copyable::@Copyable         (1.2.0)
```

The process exits, so the editor loses every language feature until the server restarts. In editors this happens constantly: any completion popup near the imports at the top of such a file kills the server.

- Deterministic: 3 out of 3 runs on 1.1.0 and on 1.2.0, on a bare `mojo-lsp-server` with no other documents open.
- Completion only: hover, definition and signatureHelp at the same position answer normally.
- The code is valid: `mojo run` compiles and runs the same file.
- Imports from other modules (`std.collections`, `std.memory`, `std.testing`) do not trigger it.

### Expected behavior

A completion list (or an empty one). The server keeps running.
