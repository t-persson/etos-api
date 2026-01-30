// Copyright Axis Communications AB.
//
// For a full list of individual contributors, please see the commit history.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package config

import (
	"flag"
	"fmt"
	"os"
)

// This is a change

// Config interface for retreiving configuration options.
type Config interface {
	ServiceHost() string
	ServicePort() string
	StripPrefix() string
	LogLevel() string
	LogFilePath() string
	ETOSNamespace() string
	DatabaseURI() string
	PublicKey() ([]byte, error)
}

// baseCfg implements the Config interface.
type baseCfg struct {
	serviceHost   string
	servicePort   string
	stripPrefix   string
	logLevel      string
	logFilePath   string
	etosNamespace string
	databaseHost  string
	databasePort  string
	publicKeyPath string
}

// load the command line vars for a base configuration.
func load() Config {
	var conf baseCfg

	flag.StringVar(&conf.serviceHost, "address", EnvOrDefault("SERVICE_HOST", "127.0.0.1"), "Address to serve API on")
	flag.StringVar(&conf.servicePort, "port", EnvOrDefault("SERVICE_PORT", "8080"), "Port to serve API on")
	flag.StringVar(&conf.stripPrefix, "stripprefix", EnvOrDefault("STRIP_PREFIX", ""), "Strip a URL prefix. Useful when a reverse proxy sets a subpath. I.e. reverse proxy sets /stream as prefix, making the etos API available at /stream/v1/events. In that case we want to set stripprefix to /stream")
	flag.StringVar(&conf.logLevel, "loglevel", EnvOrDefault("LOGLEVEL", "INFO"), "Log level (TRACE, DEBUG, INFO, WARNING, ERROR, FATAL, PANIC).")
	flag.StringVar(&conf.logFilePath, "logfilepath", os.Getenv("LOG_FILE_PATH"), "Path, including filename, for the log files to create.")
	flag.StringVar(&conf.etosNamespace, "etosnamespace", ReadNamespaceOrEnv("ETOS_NAMESPACE"), "Path, including filename, for the log files to create.")
	flag.StringVar(&conf.databaseHost, "databasehost", EnvOrDefault("ETOS_ETCD_HOST", "etcd-client"), "Host to the database.")
	flag.StringVar(&conf.databasePort, "databaseport", EnvOrDefault("ETOS_ETCD_PORT", "2379"), "Port to the database.")
	flag.StringVar(&conf.publicKeyPath, "publickeypath", os.Getenv("PUBLIC_KEY_PATH"), "Path to a public key to use for verifying JWTs.")
	return &conf
}

// ServiceHost returns the host of the service.
func (c *baseCfg) ServiceHost() string {
	return c.serviceHost
}

// ServicePort returns the port of the service.
func (c *baseCfg) ServicePort() string {
	return c.servicePort
}

// StripPrefix returns the prefix to strip. Empty string if no prefix.
func (c *baseCfg) StripPrefix() string {
	return c.stripPrefix
}

// LogLevel returns the log level.
func (c *baseCfg) LogLevel() string {
	return c.logLevel
}

// LogFilePath returns the path to where log files should be stored, including filename.
func (c *baseCfg) LogFilePath() string {
	return c.logFilePath
}

// ETOSNamespace returns the ETOS namespace.
func (c *baseCfg) ETOSNamespace() string {
	return c.etosNamespace
}

// DatabaseURI returns the URI to the ETOS database.
func (c *baseCfg) DatabaseURI() string {
	return fmt.Sprintf("%s:%s", c.databaseHost, c.databasePort)
}

// PublicKey reads a public key from disk and returns the content.
func (c *baseCfg) PublicKey() ([]byte, error) {
	if c.publicKeyPath == "" {
		return nil, nil
	}
	return os.ReadFile(c.publicKeyPath)
}

// EnvOrDefault will look up key in environment variables and return if it exists, else return the fallback value.
func EnvOrDefault(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// ReadNamespaceOrEnv checks if there's a nemspace file inside the container, else returns
// environment variable with envKey as name.
func ReadNamespaceOrEnv(envKey string) string {
	inClusterNamespace, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return os.Getenv(envKey)
	}
	return string(inClusterNamespace)
}
