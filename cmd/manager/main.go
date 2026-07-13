package main

import (
	"log/slog"
	"os"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	slog.Info("amenotejikara: not yet implemented",
		"note", "controller-runtime manager, CRD types, and Rotator implementations land in later commits")
}
