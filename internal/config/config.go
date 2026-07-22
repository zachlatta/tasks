package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	Address        string
	PublicURL      string
	Secret         string
	DatabaseURL    string
	DataDir        string
	ObjectBackend  string
	LocalObjectDir string
	S3             S3Config
}

type S3Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
	UseSSL    bool
}

func Load(dotenvPath string) (Config, error) {
	values := map[string]string{}
	if dotenvPath != "" {
		loaded, err := godotenv.Read(dotenvPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("read %s: %w", dotenvPath, err)
		}
		for key, value := range loaded {
			values[key] = value
		}
	}
	get := func(key, fallback string) string {
		if value, ok := os.LookupEnv(key); ok {
			return value
		}
		if value, ok := values[key]; ok {
			return value
		}
		return fallback
	}
	dataDirectory := get("TASK_TRACKER_DATA_DIR", "")
	if dataDirectory == "" {
		configDirectory, err := os.UserConfigDir()
		if err != nil {
			return Config{}, fmt.Errorf("find user config directory: %w", err)
		}
		dataDirectory = filepath.Join(configDirectory, "task-tracker")
	}
	address := get("TASK_TRACKER_ADDR", "127.0.0.1:8080")
	publicAddress := address
	if strings.HasPrefix(publicAddress, ":") {
		publicAddress = "127.0.0.1" + publicAddress
	}
	useSSL, err := strconv.ParseBool(get("TASK_TRACKER_S3_USE_SSL", "true"))
	if err != nil {
		return Config{}, fmt.Errorf("TASK_TRACKER_S3_USE_SSL: %w", err)
	}
	return Config{
		Address:        address,
		PublicURL:      strings.TrimRight(get("TASK_TRACKER_PUBLIC_URL", "http://"+publicAddress), "/"),
		Secret:         get("TASK_TRACKER_SECRET", ""),
		DatabaseURL:    get("TASK_TRACKER_DATABASE_URL", ""),
		DataDir:        dataDirectory,
		ObjectBackend:  strings.ToLower(get("TASK_TRACKER_OBJECT_STORE", "local")),
		LocalObjectDir: get("TASK_TRACKER_LOCAL_OBJECT_DIR", filepath.Join(dataDirectory, "images")),
		S3: S3Config{
			Endpoint:  get("TASK_TRACKER_S3_ENDPOINT", ""),
			AccessKey: get("TASK_TRACKER_S3_ACCESS_KEY", ""),
			SecretKey: get("TASK_TRACKER_S3_SECRET_KEY", ""),
			Bucket:    get("TASK_TRACKER_S3_BUCKET", ""),
			Region:    get("TASK_TRACKER_S3_REGION", ""),
			UseSSL:    useSSL,
		},
	}, nil
}

func (c Config) ValidateServer() error {
	if c.Secret == "" {
		return errors.New("TASK_TRACKER_SECRET is required to serve the web and MCP endpoints")
	}
	if c.DatabaseURL == "" {
		return errors.New("TASK_TRACKER_DATABASE_URL is required to store tasks")
	}
	parsed, err := url.Parse(c.PublicURL)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return errors.New("TASK_TRACKER_PUBLIC_URL must be an absolute origin without a path")
	}
	if parsed.Scheme != "https" {
		host := strings.ToLower(parsed.Hostname())
		if parsed.Scheme != "http" || host != "127.0.0.1" && host != "::1" && host != "localhost" {
			return errors.New("TASK_TRACKER_PUBLIC_URL must use HTTPS except on a loopback address")
		}
	}
	switch c.ObjectBackend {
	case "local":
		if c.LocalObjectDir == "" {
			return errors.New("TASK_TRACKER_LOCAL_OBJECT_DIR is required for local object storage")
		}
	case "s3":
		if c.S3.Endpoint == "" || c.S3.AccessKey == "" || c.S3.SecretKey == "" || c.S3.Bucket == "" {
			return errors.New("S3 endpoint, access key, secret key, and bucket are required for S3 object storage")
		}
	default:
		return fmt.Errorf("unsupported TASK_TRACKER_OBJECT_STORE %q", c.ObjectBackend)
	}
	return nil
}

func (c Config) SecureCookies() bool {
	parsed, _ := url.Parse(c.PublicURL)
	return parsed.Scheme == "https"
}
