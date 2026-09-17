package dhcp

import (
	"io"
	"log/slog"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
