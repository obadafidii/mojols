// Command fakelsp is a scriptable stand-in for mojo-lsp-server, for manual
// chaos testing of mojols: mojols --backend $(which fakelsp).
// See package fakelsp for the MOJOLS_FAKE_* variables.
package main

import (
	"os"

	"github.com/obadafidii/mojols/server/internal/fakelsp"
)

func main() {
	os.Exit(fakelsp.Run(os.Stdin, os.Stdout, fakelsp.OptionsFromEnv()))
}
