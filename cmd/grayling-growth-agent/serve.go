package main

import (
	"context"
	"encoding/json/v2"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
	"github.com/saker-ai/grayling/internal/growthservice"
)

type serviceFile struct {
	LocalPlannerURL string `json:"localPlannerUrl,omitempty"`
	Database        string `json:"database"`
	Address         string `json:"address"`
	BaseURL         string `json:"baseUrl"`
	Workspace       string `json:"workspace"`
	Environment     string `json:"environment"`
	ReadTokenEnv    string `json:"readTokenEnv"`
	Principals      []struct {
		Name     string `json:"name"`
		TokenEnv string `json:"tokenEnv"`
		Role     string `json:"role"`
	} `json:"principals"`
}

func runServe(ctx context.Context, args []string, out, diagnostics io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	config := fs.String("config", "", "private service configuration")
	if parseFlags(fs, args) != nil || fs.NArg() != 0 || *config == "" {
		io.WriteString(diagnostics, "invalid service configuration\n")
		return 2
	}
	// Reuse no-follow and actual-read bounds; configuration contains env names,
	// never credentials. No provider is enabled by the command.
	f, err := os.OpenFile(*config, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		io.WriteString(diagnostics, "invalid service configuration\n")
		return 2
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		io.WriteString(diagnostics, "invalid service configuration\n")
		return 2
	}
	raw, err := io.ReadAll(io.LimitReader(f, 16385))
	var c serviceFile
	if err != nil || len(raw) > 16384 || json.Unmarshal(raw, &c, json.RejectUnknownMembers(true)) != nil || !envName.MatchString(c.ReadTokenEnv) {
		io.WriteString(diagnostics, "invalid service configuration\n")
		return 2
	}
	host, _, err := net.SplitHostPort(c.Address)
	ip := net.ParseIP(host)
	// Bind only loopback. Remote access requires an operator-managed TLS proxy.
	if err != nil || ip == nil || !ip.IsLoopback() {
		io.WriteString(diagnostics, "service address must be loopback\n")
		return 2
	}
	cfg := growthservice.Config{Evidence: growth.Config{BaseURL: strings.TrimRight(c.BaseURL, "/"), WorkspaceID: c.Workspace, EnvironmentID: c.Environment, BearerToken: os.Getenv(c.ReadTokenEnv)}}
	for _, p := range c.Principals {
		if !envName.MatchString(p.TokenEnv) {
			io.WriteString(diagnostics, "invalid principal configuration\n")
			return 2
		}
		cfg.Principals = append(cfg.Principals, growthservice.Principal{Name: p.Name, Token: os.Getenv(p.TokenEnv), Role: p.Role})
	}
	if c.LocalPlannerURL != "" {
		planner, e := growthservice.NewLocalPlanner(c.LocalPlannerURL)
		if e != nil {
			io.WriteString(diagnostics, "invalid local planner configuration\n")
			return 2
		}
		cfg.Model = planner
		cfg.MaxModelCalls = 1
	}
	store, err := growthservice.Open(ctx, c.Database)
	if err != nil {
		io.WriteString(diagnostics, "service storage unavailable\n")
		return 1
	}
	defer store.Close()
	service, err := growthservice.New(ctx, store, cfg)
	if err != nil {
		io.WriteString(diagnostics, "invalid service configuration\n")
		return 2
	}
	listener, err := net.Listen("tcp", c.Address)
	if err != nil {
		service.Close()
		io.WriteString(diagnostics, "service listener unavailable\n")
		return 1
	}
	server := &http.Server{Handler: service.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); service.Run(runCtx) }()
	go func() {
		<-runCtx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
	}()
	io.WriteString(out, "growth service listening on "+listener.Addr().String()+"\n")
	err = server.Serve(listener)
	cancel()
	<-workerDone
	if err != nil && err != http.ErrServerClosed {
		io.WriteString(diagnostics, "service stopped unexpectedly\n")
		return 1
	}
	return 0
}

func runBackup(ctx context.Context, args []string, out, diagnostics io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	database := fs.String("database", "", "stopped service database")
	output := fs.String("output", "", "new private snapshot path")
	if parseFlags(fs, args) != nil || fs.NArg() != 0 || *database == "" || *output == "" {
		io.WriteString(diagnostics, "invalid backup configuration\n")
		return 2
	}
	store, err := growthservice.Open(ctx, *database)
	if err != nil {
		io.WriteString(diagnostics, "backup storage unavailable; stop service first\n")
		return 1
	}
	defer store.Close()
	if err = store.Backup(ctx, *output); err != nil {
		io.WriteString(diagnostics, "backup failed\n")
		return 1
	}
	io.WriteString(out, "growth service snapshot verified and created\n")
	return 0
}
