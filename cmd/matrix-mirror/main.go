package main

import (
	"os"

	"github.com/beeper/matrix-mirror/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
