// Copyright (c) 2019 The Jaeger Authors.
// Copyright (c) 2017 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	esV8 "github.com/elastic/go-elasticsearch/v8"
	"github.com/olivere/elastic"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zapgrpc"

	"github.com/jaegertracing/jaeger/pkg/bearertoken"
	"github.com/jaegertracing/jaeger/pkg/config/tlscfg"
	"github.com/jaegertracing/jaeger/pkg/es"
	eswrapper "github.com/jaegertracing/jaeger/pkg/es/wrapper"
	"github.com/jaegertracing/jaeger/pkg/metrics"
	storageMetrics "github.com/jaegertracing/jaeger/storage/spanstore/metrics"
)

// Configuration describes the configuration properties needed to connect to an ElasticSearch cluster
type Configuration struct {
	Servers                        []string       `mapstructure:"server_urls"`
	RemoteReadClusters             []string       `mapstructure:"remote_read_clusters"`
	Username                       string         `mapstructure:"username"`
	Password                       string         `mapstructure:"password" json:"-"`
	TokenFilePath                  string         `mapstructure:"token_file"`
	PasswordFilePath               string         `mapstructure:"password_file"`
	AllowTokenFromContext          bool           `mapstructure:"-"`
	Sniffer                        bool           `mapstructure:"sniffer"` // https://github.com/olivere/elastic/wiki/Sniffing
	SnifferTLSEnabled              bool           `mapstructure:"sniffer_tls_enabled"`
	MaxDocCount                    int            `mapstructure:"-"`                     // Defines maximum number of results to fetch from storage per query
	MaxSpanAge                     time.Duration  `yaml:"max_span_age" mapstructure:"-"` // configures the maximum lookback on span reads
	NumShards                      int64          `yaml:"shards" mapstructure:"num_shards"`
	NumReplicas                    int64          `yaml:"replicas" mapstructure:"num_replicas"`
	PrioritySpanTemplate           int64          `yaml:"priority_span_template" mapstructure:"priority_span_template"`
	PriorityServiceTemplate        int64          `yaml:"priority_service_template" mapstructure:"priority_service_template"`
	PriorityDependenciesTemplate   int64          `yaml:"priority_dependencies_template" mapstructure:"priority_dependencies_template"`
	Timeout                        time.Duration  `validate:"min=500" mapstructure:"-"`
	BulkSize                       int            `mapstructure:"-"`
	BulkWorkers                    int            `mapstructure:"-"`
	BulkActions                    int            `mapstructure:"-"`
	BulkFlushInterval              time.Duration  `mapstructure:"-"`
	IndexPrefix                    string         `mapstructure:"index_prefix"`
	IndexDateLayoutSpans           string         `mapstructure:"-"`
	IndexDateLayoutServices        string         `mapstructure:"-"`
	IndexDateLayoutDependencies    string         `mapstructure:"-"`
	IndexRolloverFrequencySpans    string         `mapstructure:"-"`
	IndexRolloverFrequencyServices string         `mapstructure:"-"`
	Tags                           TagsAsFields   `mapstructure:"tags_as_fields"`
	Enabled                        bool           `mapstructure:"-"`
	TLS                            tlscfg.Options `mapstructure:"tls"`
	UseReadWriteAliases            bool           `mapstructure:"use_aliases"`
	CreateIndexTemplates           bool           `mapstructure:"create_mappings"`
	UseILM                         bool           `mapstructure:"use_ilm"`
	Version                        uint           `mapstructure:"version"`
	LogLevel                       string         `mapstructure:"log_level"`
	SendGetBodyAs                  string         `mapstructure:"send_get_body_as"`
}

// TagsAsFields holds configuration for tag schema.
// By default Jaeger stores tags in an array of nested objects.
// This configurations allows to store tags as object fields for better Kibana support.
type TagsAsFields struct {
	// Store all tags as object fields, instead nested objects
	AllAsFields bool `mapstructure:"all"`
	// Dot replacement for tag keys when stored as object fields
	DotReplacement string `mapstructure:"dot_replacement"`
	// File path to tag keys which should be stored as object fields
	File string `mapstructure:"config_file"`
	// Comma delimited list of tags to store as object fields
	Include string `mapstructure:"include"`
}

// NewClient creates a new ElasticSearch client
func NewClient(c *Configuration, logger *zap.Logger, metricsFactory metrics.Factory) (es.Client, error) {
	if len(c.Servers) < 1 {
		return nil, errors.New("no servers specified")
	}
	options, err := c.getConfigOptions(logger)
	if err != nil {
		return nil, err
	}

	rawClient, err := elastic.NewClient(options...)
	if err != nil {
		return nil, err
	}

	sm := storageMetrics.NewWriteMetrics(metricsFactory, "bulk_index")
	m := sync.Map{}

	bulkProc, err := rawClient.BulkProcessor().
		Before(func(id int64, requests []elastic.BulkableRequest) {
			m.Store(id, time.Now())
		}).
		After(func(id int64, requests []elastic.BulkableRequest, response *elastic.BulkResponse, err error) {
			start, ok := m.Load(id)
			if !ok {
				return
			}
			m.Delete(id)

			// log individual errors, note that err might be false and these errors still present
			if response != nil && response.Errors {
				for _, it := range response.Items {
					for key, val := range it {
						if val.Error != nil {
							logger.Error("Elasticsearch part of bulk request failed", zap.String("map-key", key),
								zap.Reflect("response", val))
						}
					}
				}
			}

			sm.Emit(err, time.Since(start.(time.Time)))
			if err != nil {
				var failed int
				if response == nil {
					failed = 0
				} else {
					failed = len(response.Failed())
				}
				total := len(requests)
				logger.Error("Elasticsearch could not process bulk request",
					zap.Int("request_count", total),
					zap.Int("failed_count", failed),
					zap.Error(err),
					zap.Any("response", response))
			}
		}).
		BulkSize(c.BulkSize).
		Workers(c.BulkWorkers).
		BulkActions(c.BulkActions).
		FlushInterval(c.BulkFlushInterval).
		Do(context.Background())
	if err != nil {
		return nil, err
	}

	if c.Version == 0 {
		// Determine ElasticSearch Version
		pingResult, _, err := rawClient.Ping(c.Servers[0]).Do(context.Background())
		if err != nil {
			return nil, err
		}
		esVersion, err := strconv.Atoi(string(pingResult.Version.Number[0]))
		if err != nil {
			return nil, err
		}
		// OpenSearch is based on ES 7.x
		if strings.Contains(pingResult.TagLine, "OpenSearch") {
			if pingResult.Version.Number[0] == '1' {
				logger.Info("OpenSearch 1.x detected, using ES 7.x index mappings")
				esVersion = 7
			}
			if pingResult.Version.Number[0] == '2' {
				logger.Info("OpenSearch 2.x detected, using ES 7.x index mappings")
				esVersion = 7
			}
		}
		logger.Info("Elasticsearch detected", zap.Int("version", esVersion))
		c.Version = uint(esVersion)
	}

	var rawClientV8 *esV8.Client
	if c.Version >= 8 {
		rawClientV8, err = newElasticsearchV8(c, logger)
		if err != nil {
			return nil, fmt.Errorf("error creating v8 client: %v", err)
		}
	}

	return eswrapper.WrapESClient(rawClient, bulkProc, c.Version, rawClientV8), nil
}

func newElasticsearchV8(c *Configuration, logger *zap.Logger) (*esV8.Client, error) {
	var options esV8.Config
	options.Addresses = c.Servers
	options.Username = c.Username
	options.Password = c.Password
	options.DiscoverNodesOnStart = c.Sniffer
	transport, err := GetHTTPRoundTripper(c, logger)
	if err != nil {
		return nil, err
	}
	options.Transport = transport
	return esV8.NewClient(options)
}

// ApplyDefaults copies settings from source unless its own value is non-zero.
func (c *Configuration) ApplyDefaults(source *Configuration) {
	if len(c.RemoteReadClusters) == 0 {
		c.RemoteReadClusters = source.RemoteReadClusters
	}
	if c.Username == "" {
		c.Username = source.Username
	}
	if c.Password == "" {
		c.Password = source.Password
	}
	if !c.Sniffer {
		c.Sniffer = source.Sniffer
	}
	if c.MaxSpanAge == 0 {
		c.MaxSpanAge = source.MaxSpanAge
	}
	if c.NumShards == 0 {
		c.NumShards = source.NumShards
	}
	if c.NumReplicas == 0 {
		c.NumReplicas = source.NumReplicas
	}
	if c.PrioritySpanTemplate == 0 {
		c.PrioritySpanTemplate = source.PrioritySpanTemplate
	}
	if c.PriorityServiceTemplate == 0 {
		c.PriorityServiceTemplate = source.PriorityServiceTemplate
	}
	if c.PrioritySpanTemplate == 0 {
		c.PriorityDependenciesTemplate = source.PriorityDependenciesTemplate
	}
	if c.BulkSize == 0 {
		c.BulkSize = source.BulkSize
	}
	if c.BulkWorkers == 0 {
		c.BulkWorkers = source.BulkWorkers
	}
	if c.BulkActions == 0 {
		c.BulkActions = source.BulkActions
	}
	if c.BulkFlushInterval == 0 {
		c.BulkFlushInterval = source.BulkFlushInterval
	}
	if !c.SnifferTLSEnabled {
		c.SnifferTLSEnabled = source.SnifferTLSEnabled
	}
	if !c.Tags.AllAsFields {
		c.Tags.AllAsFields = source.Tags.AllAsFields
	}
	if c.Tags.DotReplacement == "" {
		c.Tags.DotReplacement = source.Tags.DotReplacement
	}
	if c.Tags.Include == "" {
		c.Tags.Include = source.Tags.Include
	}
	if c.Tags.File == "" {
		c.Tags.File = source.Tags.File
	}
	if c.MaxDocCount == 0 {
		c.MaxDocCount = source.MaxDocCount
	}
	if c.LogLevel == "" {
		c.LogLevel = source.LogLevel
	}
	if c.SendGetBodyAs == "" {
		c.SendGetBodyAs = source.SendGetBodyAs
	}
}

// GetIndexRolloverFrequencySpansDuration returns jaeger-span index rollover frequency duration
func (c *Configuration) GetIndexRolloverFrequencySpansDuration() time.Duration {
	if c.IndexRolloverFrequencySpans == "hour" {
		return -1 * time.Hour
	}
	return -24 * time.Hour
}

// GetIndexRolloverFrequencyServicesDuration returns jaeger-service index rollover frequency duration
func (c *Configuration) GetIndexRolloverFrequencyServicesDuration() time.Duration {
	if c.IndexRolloverFrequencyServices == "hour" {
		return -1 * time.Hour
	}
	return -24 * time.Hour
}

// TagKeysAsFields returns tags from the file and command line merged
func (c *Configuration) TagKeysAsFields() ([]string, error) {
	var tags []string

	// from file
	if c.Tags.File != "" {
		file, err := os.Open(filepath.Clean(c.Tags.File))
		if err != nil {
			return nil, err
		}

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := scanner.Text()
			if tag := strings.TrimSpace(line); tag != "" {
				tags = append(tags, tag)
			}
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
	}

	// from params
	if c.Tags.Include != "" {
		tags = append(tags, strings.Split(c.Tags.Include, ",")...)
	}

	return tags, nil
}

// getConfigOptions wraps the configs to feed to the ElasticSearch client init
func (c *Configuration) getConfigOptions(logger *zap.Logger) ([]elastic.ClientOptionFunc, error) {
	options := []elastic.ClientOptionFunc{
		elastic.SetURL(c.Servers...), elastic.SetSniff(c.Sniffer),
		// Disable health check when token from context is allowed, this is because at this time
		// we don' have a valid token to do the check ad if we don't disable the check the service that
		// uses this won't start.
		elastic.SetHealthcheck(!c.AllowTokenFromContext),
	}
	if c.SnifferTLSEnabled {
		options = append(options, elastic.SetScheme("https"))
	}
	httpClient := &http.Client{
		Timeout: c.Timeout,
	}
	options = append(options, elastic.SetHttpClient(httpClient))

	if c.Password != "" && c.PasswordFilePath != "" {
		return nil, fmt.Errorf("both Password and PasswordFilePath are set")
	}
	if c.PasswordFilePath != "" {
		passwordFromFile, err := loadTokenFromFile(c.PasswordFilePath)
		if err != nil {
			return nil, fmt.Errorf("failed to load password from file: %w", err)
		}
		c.Password = passwordFromFile
	}
	options = append(options, elastic.SetBasicAuth(c.Username, c.Password))

	if c.SendGetBodyAs != "" {
		options = append(options, elastic.SetSendGetBodyAs(c.SendGetBodyAs))
	}

	options, err := addLoggerOptions(options, c.LogLevel)
	if err != nil {
		return options, err
	}

	transport, err := GetHTTPRoundTripper(c, logger)
	if err != nil {
		return nil, err
	}
	httpClient.Transport = transport
	return options, nil
}

func addLoggerOptions(options []elastic.ClientOptionFunc, logLevel string) ([]elastic.ClientOptionFunc, error) {
	// Decouple ES logger from the log-level assigned to the parent application's log-level; otherwise, the least
	// permissive log-level will dominate.
	// e.g. --log-level=info and --es.log-level=debug would mute ES's debug logging and would require --log-level=debug
	// to show ES debug logs.
	prodConfig := zap.NewProductionConfig()

	var lvl zapcore.Level
	var setLogger func(logger elastic.Logger) elastic.ClientOptionFunc

	switch logLevel {
	case "debug":
		lvl = zap.DebugLevel
		setLogger = elastic.SetTraceLog
	case "info":
		lvl = zap.InfoLevel
		setLogger = elastic.SetInfoLog
	case "error":
		lvl = zap.ErrorLevel
		setLogger = elastic.SetErrorLog
	default:
		return options, fmt.Errorf("unrecognized log-level: \"%s\"", logLevel)
	}

	prodConfig.Level.SetLevel(lvl)
	esLogger, err := prodConfig.Build()
	if err != nil {
		return options, err
	}

	// Elastic client requires a "Printf"-able logger.
	l := zapgrpc.NewLogger(esLogger)
	options = append(options, setLogger(l))
	return options, nil
}

// GetHTTPRoundTripper returns configured http.RoundTripper
func GetHTTPRoundTripper(c *Configuration, logger *zap.Logger) (http.RoundTripper, error) {
	if c.TLS.Enabled {
		ctlsConfig, err := c.TLS.Config(logger)
		if err != nil {
			return nil, err
		}
		return &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: ctlsConfig,
		}, nil
	}
	var transport http.RoundTripper
	httpTransport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// #nosec G402
		TLSClientConfig: &tls.Config{InsecureSkipVerify: c.TLS.SkipHostVerify},
	}
	if c.TLS.CAPath != "" {
		ctlsConfig, err := c.TLS.Config(logger)
		if err != nil {
			return nil, err
		}
		httpTransport.TLSClientConfig = ctlsConfig
		transport = httpTransport
	}

	token := ""
	if c.TokenFilePath != "" {
		if c.AllowTokenFromContext {
			logger.Warn("Token file and token propagation are both enabled, token from file won't be used")
		}
		tokenFromFile, err := loadTokenFromFile(c.TokenFilePath)
		if err != nil {
			return nil, err
		}
		token = tokenFromFile
	}
	if token != "" || c.AllowTokenFromContext {
		transport = bearertoken.RoundTripper{
			Transport:       httpTransport,
			OverrideFromCtx: c.AllowTokenFromContext,
			StaticToken:     token,
		}
	}
	return transport, nil
}

func loadTokenFromFile(path string) (string, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
