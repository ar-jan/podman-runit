package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"podman-runit/internal/config"
	"podman-runit/internal/podmanapi"
	"podman-runit/internal/scheduler"
	"syscall"
	"time"
)

func main() {
	socketFlag := flag.String("socket", "", "absolute Unix socket (default PODMAN_RUNIT_SOCKET or XDG_RUNTIME_DIR/podman/podman.sock)")
	workers := flag.Int("workers", 4, "maximum concurrent checks (1..64)")
	reconcile := flag.Duration("reconcile", 60*time.Second, "full reconciliation period")
	drain := flag.Duration("drain", 8*time.Second, "shutdown drain deadline")
	flag.Parse()
	socket, err := config.Socket(*socketFlag)
	if err != nil {
		log.Fatal(err)
	}
	dir, err := config.Runtime(socket)
	if err != nil {
		log.Fatal(err)
	}
	lock, err := config.Lock(dir)
	if err != nil {
		log.Fatal(err)
	}
	defer lock.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := podmanapi.New(socket)
	defer c.Close()
	if err := scheduler.Run(ctx, c, scheduler.Options{Socket: socket, Runtime: dir, Workers: *workers, Reconcile: *reconcile, Drain: *drain}); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
