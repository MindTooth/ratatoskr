package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MindTooth/ratatoskr/internal/config"
	"github.com/MindTooth/ratatoskr/internal/service"
	"github.com/MindTooth/ratatoskr/pkg/catalog"
	"github.com/MindTooth/ratatoskr/pkg/cluster"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "refresh":
		refresh(os.Args[2:])
	case "discover":
		discover()
	case "query":
		query(os.Args[2:])
	case "channel-query":
		channelQuery(os.Args[2:])
	case "version":
		fmt.Println("ratatoskr dev")
	default:
		usage()
		os.Exit(2)
	}
}

func load(path string) (config.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return config.Config{}, err
	}
	return config.Parse(b)
}

func loadWithDigest(path string) (config.Config, [sha256.Size]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return config.Config{}, [sha256.Size]byte{}, err
	}
	c, err := config.Parse(b)
	return c, sha256.Sum256(b), err
}
func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "configuration file")
	listen := fs.String("listen", "", "HTTP listen address")
	debug := fs.Bool("debug", false, "enable debug logging")
	reloadInterval := fs.Duration("config-reload-interval", 5*time.Second, "configuration reload polling interval (0 disables reload)")
	_ = fs.Parse(args)
	if *reloadInterval < 0 {
		slog.Error("config reload interval must not be negative")
		os.Exit(2)
	}
	if *debug {
		setLogLevel(slog.LevelDebug)
	}
	c, configDigest, err := loadWithDigest(*configPath)
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	if c.Debug {
		setLogLevel(slog.LevelDebug)
	}
	if *listen == "" {
		*listen = c.ListenAddress
	}
	if *listen == "" {
		*listen = ":8080"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	svc := service.New(c.Service)
	go svc.Run(ctx)
	if *reloadInterval > 0 {
		go watchConfig(ctx, *configPath, *reloadInterval, configDigest, func(next config.Config) {
			if next.ListenAddress != c.ListenAddress {
				slog.Warn("listen address change requires restart", "configured", next.ListenAddress)
			}
			if *debug || next.Debug {
				setLogLevel(slog.LevelDebug)
			} else {
				setLogLevel(slog.LevelInfo)
			}
			svc.Reload(next.Service)
			c = next
			slog.Info("config reloaded", "sources", len(next.Service.Sources))
		})
	}
	server := &http.Server{Addr: *listen, Handler: svc.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("serving", "address", *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server", "error", err)
		os.Exit(1)
	}
}

// watchConfig compares file content instead of filesystem events. This also
// works for Kubernetes ConfigMap mounts, which atomically swap symlinks.
func watchConfig(ctx context.Context, configPath string, interval time.Duration, last [sha256.Size]byte, onChange func(config.Config)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b, err := os.ReadFile(configPath)
			if err != nil {
				slog.Error("read config for reload", "error", err)
				continue
			}
			digest := sha256.Sum256(b)
			if digest == last {
				continue
			}
			last = digest
			c, err := config.Parse(b)
			if err != nil {
				slog.Error("reload config", "error", err)
				continue
			}
			onChange(c)
		}
	}
}

func setLogLevel(level slog.Level) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}
func refresh(args []string) {
	fs := flag.NewFlagSet("refresh", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "configuration file")
	sourceID := fs.String("source", "", "source ID")
	_ = fs.Parse(args)
	c, err := load(*configPath)
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	svc := service.New(c.Service)
	for _, source := range c.Service.Sources {
		if *sourceID != "" && source.ID != *sourceID {
			continue
		}
		if err := svc.Refresh(context.Background(), source); err != nil {
			slog.Error("refresh", "source", source.ID, "error", err)
			os.Exit(1)
		}
		fmt.Println(source.ID)
	}
}
func discover() {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		slog.Error("load in-cluster configuration", "error", err)
		os.Exit(1)
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		slog.Error("create Kubernetes client", "error", err)
		os.Exit(1)
	}
	subscriptions, err := cluster.Discover(context.Background(), client)
	if err != nil {
		slog.Error("discover subscriptions", "error", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(subscriptions); err != nil {
		slog.Error("write discovery response", "error", err)
		os.Exit(1)
	}
}

func query(args []string) {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	imageRef := fs.String("image", "", "catalog image")
	packageName := fs.String("package", "", "operator package")
	channel := fs.String("channel", "", "channel")
	currentVersion := fs.String("current-version", "", "installed version")
	currentBundle := fs.String("current-bundle", "", "installed bundle")
	policy := fs.String("signature-policy", "", "containers/image policy")
	platform := fs.String("platform", config.DefaultPlatform, "catalog platform (for example linux/amd64)")
	_ = fs.Parse(args)
	if *imageRef == "" || *packageName == "" || (*currentVersion == "" && *currentBundle == "") {
		fs.Usage()
		os.Exit(2)
	}
	snap, err := (catalog.Reader{SignaturePolicy: *policy}).Read(context.Background(), catalog.Source{ID: "query", Image: *imageRef, Platform: *platform})
	if err != nil {
		slog.Error("read catalog", "error", err)
		os.Exit(1)
	}
	pkg := snap.Packages[*packageName]
	if pkg == nil {
		slog.Error("unknown package", "package", *packageName)
		os.Exit(1)
	}
	versions, err := pkg.VersionUpdates(catalog.UpdateRequest{CurrentVersion: *currentVersion, CurrentBundle: *currentBundle, Channel: *channel, Mode: "reachable"})
	if err != nil {
		slog.Error("resolve update", "error", err)
		os.Exit(1)
	}
	writeReleases(versions)
}

func channelQuery(args []string) {
	fs := flag.NewFlagSet("channel-query", flag.ExitOnError)
	imageRef := fs.String("image", "", "catalog image")
	packageName := fs.String("package", "", "operator package")
	currentChannel := fs.String("current-channel", "", "installed channel")
	currentVersion := fs.String("current-version", "", "installed version")
	currentBundle := fs.String("current-bundle", "", "installed bundle")
	policy := fs.String("signature-policy", "", "containers/image policy")
	platform := fs.String("platform", config.DefaultPlatform, "catalog platform (for example linux/amd64)")
	_ = fs.Parse(args)
	if *imageRef == "" || *packageName == "" || *currentChannel == "" || (*currentVersion == "" && *currentBundle == "") {
		fs.Usage()
		os.Exit(2)
	}
	snap, err := (catalog.Reader{SignaturePolicy: *policy}).Read(context.Background(), catalog.Source{ID: "query", Image: *imageRef, Platform: *platform})
	if err != nil {
		slog.Error("read catalog", "error", err)
		os.Exit(1)
	}
	pkg := snap.Packages[*packageName]
	if pkg == nil {
		slog.Error("unknown package", "package", *packageName)
		os.Exit(1)
	}
	channels, err := pkg.ChannelUpdates(catalog.ChannelUpdateRequest{CurrentChannel: *currentChannel, CurrentVersion: *currentVersion, CurrentBundle: *currentBundle, Selection: "next"})
	if err != nil {
		slog.Error("resolve channel", "error", err)
		os.Exit(1)
	}
	writeReleases(channels)
}

func writeReleases(values []string) {
	out := struct {
		Releases []struct {
			Version string `json:"version"`
		} `json:"releases"`
	}{Releases: make([]struct {
		Version string `json:"version"`
	}, len(values))}
	for i, value := range values {
		out.Releases[i].Version = value
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		slog.Error("write response", "error", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ratatoskr {serve|refresh|discover|query|channel-query|version}")
}
