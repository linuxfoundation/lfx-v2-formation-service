// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package config provides application configuration loaded from CLI flags and environment variables.
package config

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
)

// Config holds application configuration.
type Config struct {
	Host  string
	Port  string
	Debug bool

	JWKSUrl  string
	Audience string
	Issuer   string

	NATSUrl string

	Database DatabaseConfig

	// ReconcileInterval is how often the reconcile loop sweeps for missing
	// checklists and stale projections.
	ReconcileInterval time.Duration
}

// DatabaseConfig holds the five credential values the provisioned secret
// carries, plus the SSL mode. The DSN is composed from them in process
// rather than read as a single URL, so the password is never part of a value
// that could be logged or surfaced whole.
type DatabaseConfig struct {
	Host     string
	Port     string
	Username string
	Password string
	DBName   string
	SSLMode  string
}

// DSN composes a libpq keyword/value connection string. Every value is
// quoted (libpqQuote), because unquoted keyword/value pairs are whitespace-
// delimited: a generated password containing a space, single quote or
// backslash would otherwise be misparsed as extra parameters or corrupt the
// string entirely. sslmode is omitted when unset, leaving pgx's own default
// (prefer) in effect.
func (d DatabaseConfig) DSN() string {
	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s",
		libpqQuote(d.Host), libpqQuote(d.Port), libpqQuote(d.Username), libpqQuote(d.Password), libpqQuote(d.DBName))
	if d.SSLMode != "" {
		dsn += " sslmode=" + libpqQuote(d.SSLMode)
	}
	return dsn
}

// libpqQuote quotes a libpq keyword/value field per the format's own escaping
// rules: wrap in single quotes, and backslash-escape any single quote or
// backslash already in the value. Always quoting (even values with no
// special characters) keeps the DSN uniform rather than conditional.
func libpqQuote(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return "'" + v + "'"
}

// Redacted returns the DSN with the password replaced, for logging.
func (d DatabaseConfig) Redacted() string {
	dsn := fmt.Sprintf("host=%s port=%s user=%s dbname=%s",
		d.Host, d.Port, d.Username, d.DBName)
	if d.SSLMode != "" {
		dsn += " sslmode=" + d.SSLMode
	}
	return dsn
}

// LoadConfig loads configuration from CLI flags, then environment variables, then defaults.
// Priority: CLI flags > env vars > defaults.
func LoadConfig() *Config {
	slog.Info("loading application configuration")

	defaultPort := os.Getenv(constants.EnvPort)
	if defaultPort == "" {
		defaultPort = constants.DefaultHTTPPort
	}
	defaultHost := os.Getenv(constants.EnvHost)
	if defaultHost == "" {
		defaultHost = constants.DefaultHost
	}

	portF := flag.String("p", defaultPort, "listen port")
	hostF := flag.String("bind", defaultHost, "interface to bind on")
	dbgF := flag.Bool("d", false, "enable debug logging")
	flag.Parse()

	cfg := &Config{
		Port:     *portF,
		Host:     *hostF,
		Debug:    *dbgF,
		JWKSUrl:  envOrDefault(constants.EnvJWKSURL, constants.DefaultJWKSURL),
		Audience: envOrDefault(constants.EnvAudience, constants.DefaultAudience),
		Issuer:   envOrDefault(constants.EnvIssuer, constants.DefaultIssuer),
		NATSUrl:  envOrDefault(constants.EnvNATSURL, constants.DefaultNATSURL),
		Database: DatabaseConfig{
			Host:     envOrDefault(constants.EnvDBHost, constants.DefaultDBHost),
			Port:     envOrDefault(constants.EnvDBPort, constants.DefaultDBPort),
			Username: os.Getenv(constants.EnvDBUsername),
			Password: os.Getenv(constants.EnvDBPassword),
			DBName:   envOrDefault(constants.EnvDBName, constants.DefaultDBName),
			SSLMode:  envOrDefault(constants.EnvDBSSLMode, constants.DefaultDBSSLMode),
		},
		ReconcileInterval: durationOrDefault(constants.EnvReconcileInterval, constants.DefaultReconcileInterval),
	}

	if os.Getenv(constants.EnvDebug) == "true" {
		cfg.Debug = true
	}

	return cfg
}

// ServerAddress returns the address the HTTP server should bind to.
func (c *Config) ServerAddress() string {
	if c.Host == "*" {
		return ":" + c.Port
	}
	return c.Host + ":" + c.Port
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// durationOrDefault falls back to def on an unset or unparseable value,
// logging the latter rather than failing startup over a tunable.
func durationOrDefault(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("ignoring unparseable duration, using default",
			"env", key, "value", v, "default", def.String())
		return def
	}
	return d
}
