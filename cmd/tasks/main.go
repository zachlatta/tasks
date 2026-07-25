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
	"strconv"
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
	"github.com/zachlatta/tasks/internal/taskapi"
	"github.com/zachlatta/tasks/internal/web"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
	case "add", "edit", "query", "done", "delete", "restore", "serve":
		// These commands operate on stored tasks and need configuration below.
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
	if args[0] == "serve" {
		return runServerCommand(args, loaded, stdout, stderr)
	}
	if strings.TrimSpace(loaded.APIURL) == "" {
		fmt.Fprintln(stderr, "configuration: TASKS_API_URL is required")
		return 1
	}
	if strings.TrimSpace(loaded.Secret) == "" {
		fmt.Fprintln(stderr, "configuration: TASKS_SECRET is required")
		return 1
	}
	client, err := taskapi.NewClient(loaded.APIURL, loaded.Secret)
	if err != nil {
		fmt.Fprintf(stderr, "configuration: %v\n", err)
		return 1
	}
	return runClientCommand(args, stdin, stdout, stderr, client)
}

func runClientCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, client *taskapi.Client) int {
	switch args[0] {
	case "add":
		flags := flag.NewFlagSet("add", flag.ContinueOnError)
		flags.SetOutput(stderr)
		description := flags.String("description", "", "Markdown description")
		dependencies := flags.String("depends-on", "", "comma-separated dependency task IDs")
		wakeAt := flags.String("wake-at", "", "hold the task off the board until this date (YYYY-MM-DD or RFC 3339)")
		waitingOn := flags.String("waiting-on", "", "one line naming who or what the task is waiting for")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		title := strings.Join(flags.Args(), " ")
		created, err := client.CreateTask(context.Background(), taskapi.CreateTaskInput{
			Title:        title,
			Description:  *description,
			Dependencies: strings.Split(*dependencies, ","),
			WakeAt:       *wakeAt,
			WaitingOn:    *waitingOn,
		})
		if err != nil {
			fmt.Fprintf(stderr, "add task: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "created %s\n", created.ID)
		return 0
	case "edit":
		flags := flag.NewFlagSet("edit", flag.ContinueOnError)
		flags.SetOutput(stderr)
		var title optionalString
		var description optionalString
		var descriptionFile optionalString
		var dependencies optionalString
		var wakeAt optionalString
		var waitingOn optionalString
		var expectedVersion optionalPositiveInt64
		flags.Var(&title, "title", "complete replacement title")
		flags.Var(&description, "description", "complete replacement Markdown description")
		flags.Var(&descriptionFile, "description-file", "read replacement Markdown description from a file, or - for stdin")
		flags.Var(&dependencies, "depends-on", "complete replacement comma-separated dependency task IDs; empty clears")
		flags.Var(&wakeAt, "wake-at", "hold the task off the board until this date (YYYY-MM-DD or RFC 3339); empty wakes it now")
		flags.Var(&waitingOn, "waiting-on", "one line naming who or what the task is waiting for; empty clears")
		flags.Var(&expectedVersion, "expected-version", "fail if the stored task version differs")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if len(flags.Args()) != 1 {
			fmt.Fprintln(stderr, "Usage: tasks edit [--title text] [--description text | --description-file path|-] [--depends-on id,id] [--wake-at date] [--waiting-on text] [--expected-version n] <task-id>")
			return 2
		}
		if description.set && descriptionFile.set {
			fmt.Fprintln(stderr, "edit task: --description and --description-file cannot be used together")
			return 2
		}
		if !title.set && !description.set && !descriptionFile.set && !dependencies.set && !wakeAt.set && !waitingOn.set {
			fmt.Fprintln(stderr, "edit task: at least one of --title, --description, --description-file, --depends-on, --wake-at, or --waiting-on is required")
			return 2
		}

		input := taskapi.UpdateTaskInput{ID: flags.Args()[0]}
		if title.set {
			input.Title = &title.value
		}
		if description.set {
			input.Description = &description.value
		}
		if descriptionFile.set {
			value, err := readTextInput(stdin, descriptionFile.value)
			if err != nil {
				fmt.Fprintf(stderr, "edit task: read description: %v\n", err)
				return 1
			}
			input.Description = &value
		}
		if dependencies.set {
			values := strings.Split(dependencies.value, ",")
			input.Dependencies = &values
		}
		if wakeAt.set {
			input.WakeAt = &wakeAt.value
		}
		if waitingOn.set {
			input.WaitingOn = &waitingOn.value
		}
		if expectedVersion.set {
			input.ExpectedVersion = &expectedVersion.value
		}
		edited, err := client.UpdateTask(context.Background(), input)
		if err != nil {
			fmt.Fprintf(stderr, "edit task: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "edited %s (version %d)\n", edited.ID, edited.Version)
		return 0
	case "query":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "Usage: tasks query <read-only-sql>")
			return 2
		}
		result, err := client.QueryTasksSQL(context.Background(), taskapi.SQLQueryInput{
			SQL: strings.Join(args[1:], " "),
		})
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
		completed, err := client.CompleteTask(context.Background(), taskapi.CompleteTaskInput{ID: args[1]})
		if err != nil {
			fmt.Fprintf(stderr, "complete task: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "completed %s\n", completed.ID)
		return 0
	case "delete":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "Usage: tasks delete <task-id>")
			return 2
		}
		deleted, err := client.DeleteTask(context.Background(), taskapi.DeleteTaskInput{ID: args[1]})
		if err != nil {
			fmt.Fprintf(stderr, "delete task: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "deleted %s\n", deleted.ID)
		return 0
	case "restore":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "Usage: tasks restore <task-id>")
			return 2
		}
		restored, err := client.RestoreTask(context.Background(), taskapi.RestoreTaskInput{ID: args[1]})
		if err != nil {
			fmt.Fprintf(stderr, "restore task: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "restored %s\n", restored.ID)
		return 0
	}
	return 0
}

func runServerCommand(args []string, loaded config.Config, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "Usage: tasks serve")
		return 2
	}
	if err := loaded.ValidateServer(); err != nil {
		fmt.Fprintf(stderr, "configuration: %v\n", err)
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
	if err := serve(loaded, service, store, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "serve: %v\n", err)
		return 1
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
	tools := taskapi.NewTools(service, store)
	handler, err := app.NewHTTPHandler(
		webHandler,
		oauthServer,
		mcpserver.NewWithTools(tools, version),
		taskapi.NewHandler(tools),
		loaded.PublicURL,
	)
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

type optionalString struct {
	value string
	set   bool
}

func (value *optionalString) String() string {
	return value.value
}

func (value *optionalString) Set(input string) error {
	value.value = input
	value.set = true
	return nil
}

type optionalPositiveInt64 struct {
	value int64
	set   bool
}

func (value *optionalPositiveInt64) String() string {
	if !value.set {
		return ""
	}
	return strconv.FormatInt(value.value, 10)
}

func (value *optionalPositiveInt64) Set(input string) error {
	parsed, err := strconv.ParseInt(input, 10, 64)
	if err != nil {
		return fmt.Errorf("must be a positive integer: %w", err)
	}
	if parsed < 1 {
		return errors.New("must be a positive integer")
	}
	value.value = parsed
	value.set = true
	return nil
}

func readTextInput(stdin io.Reader, path string) (string, error) {
	var (
		content []byte
		err     error
	)
	if path == "-" {
		content, err = io.ReadAll(stdin)
	} else {
		content, err = os.ReadFile(path)
	}
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func usage(output io.Writer) {
	fmt.Fprintln(output, `Usage:
  tasks add [--description text] [--depends-on id,id] [--wake-at date] [--waiting-on text] <title>
  tasks edit [--title text] [--description text | --description-file path|-] [--depends-on id,id] [--wake-at date] [--waiting-on text] [--expected-version n] <task-id>
  tasks query <read-only-sql>
  tasks done <task-id>
  tasks delete <task-id>
  tasks restore <task-id>
  tasks serve
  tasks version`)
}
