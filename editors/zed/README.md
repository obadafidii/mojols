# mojols for Zed

Mojo language support for Zed, backed by mojols.

The extension finds `mojols` in this order: `lsp.mojols.binary.path` in settings,
`mojols` on the worktree's PATH, a copy it downloaded before, then the latest
GitHub release (`mojols_<os>_<arch>.tar.gz`). mojols then finds `mojo-lsp-server`
itself (`mojols doctor` explains how).

Install as a dev extension: `zed: extensions` → Install Dev Extension → this folder.
Uninstall other Mojo extensions first; two extensions cannot both define Mojo.

Grammar: vadim-su/tree-sitter-mojo, pinned by commit. Queries and snippets are
from vadim-su/zed_mojo (MIT, see LICENSE-queries-MIT).
