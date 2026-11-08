# mojols for CLion (M4)

Not started yet: the plugin comes after the server is robust (M1) and
workspace-aware (M3). It will stay thin, like the Zed extension: find or
download `mojols`, start it, pass settings.

Decisions already made (see the design doc):

- Built on JetBrains' built-in LSP API, in Kotlin with the IntelliJ Platform Gradle plugin.
- Register through `LspIntegrationProvider` (2026.1.4+; `LspServerSupportProvider` before),
  with a `ProjectWideLspClientDescriptor` whose `createCommandLine()` runs `mojols serve`.
- `plugin.xml` depends on `com.intellij.modules.lsp` and `com.intellij.modules.ultimate`.
- Minimum CLion 2026.1: LSP rename, code lens and range formatting landed there.
- JetBrains is open-sourcing this client API in the 2026.2 cycle (`LspServer` becomes
  `LspClient`); build against the new names.
- Reference: https://plugins.jetbrains.com/docs/intellij/language-server-protocol.html
