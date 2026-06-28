package main

import (
	// Embed the IANA tz database so time.LoadLocation works in the static (CGO-disabled) binary
	// even on a minimal runtime image without /usr/share/zoneinfo — needed to write recurring
	// overrides/splits in the series' TZID.
	_ "time/tzdata"

	"github.com/roman-16/proton-cli/internal/cli"
)

func main() {
	cli.Execute()
}
