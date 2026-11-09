macOS (where the crash was found):

- Mojo 1.1.0 (8189361e), installed with pip into `~/.mojo` rather than a pixi project, so `pixi list max` has nothing to show.
- macOS 26.6.2, Apple M4 (osx-arm64). `repro_lsp.py` reproduces: same `RegisterPassable` abort and stack dump.

```
$ pixi info
System
------------
       Pixi version: 0.81.0
        TLS backend: rustls
           Platform: osx-arm64
   Virtual packages: __unix=0=0
                   : __osx=26.6.2=0
                   : __archspec=1=m4
```

Also reproduced on Linux:

- Ubuntu 24.04.5 LTS, x86_64, Mojo 1.1.0 (pip packages mojo 1.1.0, mojo-compiler 1.1.0).
- Ubuntu 24.04.5 LTS, x86_64, Mojo 1.2.0 (2016d0cd): still crashes, now reporting `@std::@traits::@copyable::@Copyable`. The macOS-only mid-identifier crashes have not been tested on 1.2.0 yet.
