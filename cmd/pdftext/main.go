// Command pdftext is the sandboxed PDF text-extraction helper that runs as a
// sidecar of mail-mcp. See package internal/pdftext.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/excavador/mail-mcp/internal/pdftext"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == pdftext.ChildMode {
		pdftext.ExitChild(os.Args[2:])
		return
	}
	socket := flag.String("socket", "/run/pdftext/pdftext.sock", "unix socket to listen on")
	root := flag.String("root", "/store", "read-only blob store root (blobs are root/pdf/<hh>/<sha256>)")
	prog := flag.String("pdftotext", "/usr/bin/pdftotext", "pdftotext binary")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	r, err := pdftext.NewRunner(pdftext.Config{Root: *root, Program: *prog})
	if err != nil {
		fmt.Fprintln(os.Stderr, "pdftext:", err)
		os.Exit(1)
	}
	ln, err := pdftext.Listen(*socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pdftext: listen:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); _ = ln.Close() }()
	log.Info("pdftext listening", "socket", *socket, "root", *root)
	if err := pdftext.Serve(ln, r, log); err != nil {
		fmt.Fprintln(os.Stderr, "pdftext:", err)
		os.Exit(1)
	}
}
