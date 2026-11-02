// grayling-growth-lab serves an owned, synthetic business application locally.
package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/saker-ai/grayling/internal/growthlab"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	dir := flag.String("data-dir", "", "owned private absolute directory")
	address := flag.String("address", "127.0.0.1:0", "literal loopback listener")
	synthetic := flag.Bool("synthetic", false, "acknowledge synthetic identities, traffic, clock and outcomes")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	lab, e := growthlab.Start(ctx, growthlab.Options{Directory: *dir, Address: *address, Synthetic: *synthetic})
	if e != nil {
		fmt.Fprintln(os.Stderr, "synthetic lab could not start; inspect private configuration")
		os.Exit(1)
	}
	defer lab.Close()
	fmt.Println("synthetic lab listening on " + lab.URL())
	<-ctx.Done()
}
