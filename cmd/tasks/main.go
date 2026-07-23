package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zachlatta/tasks/internal/app"
	"github.com/zachlatta/tasks/internal/auth"
	"github.com/zachlatta/tasks/internal/config"
	"github.com/zachlatta/tasks/internal/mcpserver"
	"github.com/zachlatta/tasks/internal/objectstore"
	"github.com/zachlatta/tasks/internal/postgres"
	"github.com/zachlatta/tasks/internal/task"
	"github.com/zachlatta/tasks/internal/web"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "--help", "-h":
		usage(stdout)
		return 0
	case "add", "query", "done", "serve":
		// These commands operate on stored tasks and need the database below.
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
	loaded, err := config.Load(".env")
	if err != nil {
		fmt.Fprintf(stderr, "configuration: %v\n", err)
		return 1
	}
	if strings.TrimSpace(loaded.DatabaseURL) == "" {
		fmt.Fprintln(stderr, "configuration: TASKS_DATABASE_URL is required")
		return 1
	}
	store, err := postgres.Open(context.Background(), loaded.DatabaseURL)
	if err != nil {
		fmt.Fprintf(stderr, "database: %v\n", err)
		return 1
	}
	defer store.Close()
	service := task.NewService(store, time.Now, func() string {
		return strings.ToLower(rand.Text())
	})
	mutationContext := task.WithAuditMetadata(context.Background(), task.AuditMetadata{
		ActorKind: "local_user",
		Source:    "cli",
	})
	switch args[0] {
	case "add":
		flags := flag.NewFlagSet("add", flag.ContinueOnError)
		flags.SetOutput(stderr)
		description := flags.String("description", "", "Markdown description")
		dependencies := flags.String("depends-on", "", "comma-separated dependency task IDs")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		title := strings.Join(flags.Args(), " ")
		created, err := service.Create(mutationContext, task.CreateInput{
			Title: title, Description: *description, Dependencies: strings.Split(*dependencies, ","),
		})
		if err != nil {
			fmt.Fprintf(stderr, "add task: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "created %s\n", created.ID)
		return 0
	case "query":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "Usage: tasks query <read-only-sql>")
			return 2
		}
		result, err := store.Query(context.Background(), strings.Join(args[1:], " "))
		if err != nil {
			fmt.Fprintf(stderr, "query tasks: %v\n", err)
			return 1
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(result); err != nil {
			fmt.Fprintf(stderr, "encode query result: %v\n", err)
			return 1
		}
		return 0
	case "done":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "Usage: tasks done <task-id>")
			return 2
		}
		completed, err := service.Complete(mutationContext, args[1])
		if err != nil {
			fmt.Fprintf(stderr, "complete task: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "completed %s\n", completed.ID)
		return 0
	case "serve":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "Usage: tasks serve")
			return 2
		}
		if err := loaded.ValidateServer(); err != nil {
			fmt.Fprintf(stderr, "configuration: %v\n", err)
			return 1
		}
		if err := serve(loaded, service, store, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "serve: %v\n", err)
			return 1
		}
		return 0
	}
	return 0
}

func serve(loaded config.Config, service *task.Service, store *postgres.Store, stdout, stderr io.Writer) error {
	objects, err := configuredObjectStore(loaded)
	if err != nil {
		return err
	}
	oauthServer := auth.NewServer(auth.Config{Issuer: loaded.PublicURL, Secret: loaded.Secret, Store: store})
	webHandler := web.New(web.Config{
		Tasks: service, Reader: store, Objects: objects, Auth: oauthServer, SecureCookies: loaded.SecureCookies(), Sessions: store,
	})
	handler, err := app.NewHTTPHandler(webHandler, oauthServer, mcpserver.New(service, store, version), loaded.PublicURL)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              loaded.Address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          log.New(stderr, "http: ", log.LstdFlags),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweepCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := store.DeleteExpiredAuthState(sweepCtx, time.Now()); err != nil {
					fmt.Fprintf(stderr, "cleanup expired auth state: %v\n", err)
				}
				cancel()
			}
		}
	}()
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- httpServer.ListenAndServe()
	}()
	fmt.Fprintf(stdout, "tasks %s listening on %s (public URL %s)\n", version, loaded.Address, loaded.PublicURL)
	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownContext)
	}
}

func configuredObjectStore(loaded config.Config) (objectstore.Store, error) {
	if loaded.ObjectBackend == "local" {
		return objectstore.NewLocal(loaded.LocalObjectDir), nil
	}
	return objectstore.NewS3(objectstore.S3Config{
		Endpoint: loaded.S3.Endpoint, AccessKey: loaded.S3.AccessKey, SecretKey: loaded.S3.SecretKey,
		Bucket: loaded.S3.Bucket, Region: loaded.S3.Region, UseSSL: loaded.S3.UseSSL,
	})
}

func usage(output io.Writer) {
	fmt.Fprintln(output, `Usage:
  tasks add [--description text] [--depends-on id,id] <title>
  tasks query <read-only-sql>
  tasks done <task-id>
  tasks serve
  tasks version`)
}
