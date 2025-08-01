// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouseexporter // import "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter"

import (
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configopaque"
	"go.opentelemetry.io/collector/config/configretry"
	"go.opentelemetry.io/collector/exporter/exporterhelper"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter/internal"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter/internal/metrics"
)

// Config defines configuration for clickhouse exporter.
type Config struct {
	// collectorVersion is the build version of the collector. This is overridden when an exporter is initialized.
	collectorVersion string

	TimeoutSettings           exporterhelper.TimeoutConfig `mapstructure:",squash"`
	configretry.BackOffConfig `mapstructure:"retry_on_failure"`
	QueueSettings             exporterhelper.QueueBatchConfig `mapstructure:"sending_queue"`

	// Endpoint is the clickhouse endpoint.
	Endpoint string `mapstructure:"endpoint"`
	// Username is the authentication username.
	Username string `mapstructure:"username"`
	// Password is the authentication password.
	Password configopaque.String `mapstructure:"password"`
	// Database is the database name to export.
	Database string `mapstructure:"database"`
	// ConnectionParams is the extra connection parameters with map format. for example compression/dial_timeout
	ConnectionParams map[string]string `mapstructure:"connection_params"`
	// LogsTableName is the table name for logs. default is `otel_logs`.
	LogsTableName string `mapstructure:"logs_table_name"`
	// TracesTableName is the table name for traces. default is `otel_traces`.
	TracesTableName string `mapstructure:"traces_table_name"`
	// MetricsTableName is the table name for metrics. default is `otel_metrics`.
	//
	// Deprecated: MetricsTableName exists for historical compatibility
	// and should not be used. To set the metrics tables name,
	// use the MetricsTables parameter instead.
	MetricsTableName string `mapstructure:"metrics_table_name"`
	// TTL is The data time-to-live example 30m, 48h. 0 means no ttl.
	TTL time.Duration `mapstructure:"ttl"`
	// TableEngine is the table engine to use. default is `MergeTree()`.
	TableEngine TableEngine `mapstructure:"table_engine"`
	// ClusterName if set will append `ON CLUSTER` with the provided name when creating tables.
	ClusterName string `mapstructure:"cluster_name"`
	// CreateSchema if set to true will run the DDL for creating the database and tables. default is true.
	CreateSchema bool `mapstructure:"create_schema"`
	// Compress controls the compression algorithm. Valid options: `none` (disabled), `zstd`, `lz4` (default), `gzip`, `deflate`, `br`, `true` (lz4).
	Compress string `mapstructure:"compress"`
	// AsyncInsert if true will enable async inserts. Default is `true`.
	// Ignored if async inserts are configured in the `endpoint` or `connection_params`.
	// Async inserts may still be overridden server-side.
	AsyncInsert bool `mapstructure:"async_insert"`
	// MetricsTables defines the table names for metric types.
	MetricsTables MetricTablesConfig `mapstructure:"metrics_tables"`
}

type MetricTablesConfig struct {
	// Gauge is the table name for gauge metric type. default is `otel_metrics_gauge`.
	Gauge metrics.MetricTypeConfig `mapstructure:"gauge"`
	// Sum is the table name for sum metric type. default is `otel_metrics_sum`.
	Sum metrics.MetricTypeConfig `mapstructure:"sum"`
	// Summary is the table name for summary metric type. default is `otel_metrics_summary`.
	Summary metrics.MetricTypeConfig `mapstructure:"summary"`
	// Histogram is the table name for histogram metric type. default is `otel_metrics_histogram`.
	Histogram metrics.MetricTypeConfig `mapstructure:"histogram"`
	// ExponentialHistogram is the table name for exponential histogram metric type. default is `otel_metrics_exponential_histogram`.
	ExponentialHistogram metrics.MetricTypeConfig `mapstructure:"exponential_histogram"`
}

// TableEngine defines the ENGINE string value when creating the table.
type TableEngine struct {
	Name   string `mapstructure:"name"`
	Params string `mapstructure:"params"`
}

const (
	defaultDatabase           = "default"
	defaultTableEngineName    = "MergeTree"
	defaultMetricTableName    = "otel_metrics"
	defaultGaugeSuffix        = "_gauge"
	defaultSumSuffix          = "_sum"
	defaultSummarySuffix      = "_summary"
	defaultHistogramSuffix    = "_histogram"
	defaultExpHistogramSuffix = "_exponential_histogram"
)

var (
	errConfigNoEndpoint      = errors.New("endpoint must be specified")
	errConfigInvalidEndpoint = errors.New("endpoint must be url format")
)

func createDefaultConfig() component.Config {
	return &Config{
		collectorVersion: "unknown",

		TimeoutSettings:  exporterhelper.NewDefaultTimeoutConfig(),
		QueueSettings:    exporterhelper.NewDefaultQueueConfig(),
		BackOffConfig:    configretry.NewDefaultBackOffConfig(),
		ConnectionParams: map[string]string{},
		Database:         defaultDatabase,
		LogsTableName:    "otel_logs",
		TracesTableName:  "otel_traces",
		TTL:              0,
		CreateSchema:     true,
		AsyncInsert:      true,
		MetricsTables: MetricTablesConfig{
			Gauge:                metrics.MetricTypeConfig{Name: defaultMetricTableName + defaultGaugeSuffix},
			Sum:                  metrics.MetricTypeConfig{Name: defaultMetricTableName + defaultSumSuffix},
			Summary:              metrics.MetricTypeConfig{Name: defaultMetricTableName + defaultSummarySuffix},
			Histogram:            metrics.MetricTypeConfig{Name: defaultMetricTableName + defaultHistogramSuffix},
			ExponentialHistogram: metrics.MetricTypeConfig{Name: defaultMetricTableName + defaultExpHistogramSuffix},
		},
	}
}

// Validate the ClickHouse server configuration.
func (cfg *Config) Validate() (err error) {
	if cfg.Endpoint == "" {
		err = errors.Join(err, errConfigNoEndpoint)
	}

	dsn, e := cfg.buildDSN()
	if e != nil {
		err = errors.Join(err, e)
	}

	cfg.buildMetricTableNames()

	// Validate DSN with clickhouse driver.
	// Last chance to catch invalid config.
	if _, e := clickhouse.ParseDSN(dsn); e != nil {
		err = errors.Join(err, e)
	}

	return err
}

func (cfg *Config) buildDSN() (string, error) {
	dsnURL, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return "", fmt.Errorf("%w: %s", errConfigInvalidEndpoint, err.Error())
	}

	queryParams := dsnURL.Query()

	// Add connection params to query params.
	for k, v := range cfg.ConnectionParams {
		queryParams.Set(k, v)
	}

	// Enable TLS if scheme is https. This flag is necessary to support https connections.
	if dsnURL.Scheme == "https" {
		queryParams.Set("secure", "true")
	}

	// Use async_insert from config if not specified in DSN.
	if !queryParams.Has("async_insert") {
		queryParams.Set("async_insert", fmt.Sprintf("%t", cfg.AsyncInsert))
	}

	if !queryParams.Has("compress") && (cfg.Compress == "" || cfg.Compress == "true") {
		queryParams.Set("compress", "lz4")
	} else if !queryParams.Has("compress") {
		queryParams.Set("compress", cfg.Compress)
	}

	productInfo := queryParams.Get("client_info_product")
	collectorProductInfo := fmt.Sprintf("%s/%s", "otelcol", cfg.collectorVersion)
	if productInfo == "" {
		productInfo = collectorProductInfo
	} else {
		productInfo = fmt.Sprintf("%s,%s", productInfo, collectorProductInfo)
	}
	queryParams.Set("client_info_product", productInfo)

	// Override username and password if specified in config.
	if cfg.Username != "" {
		dsnURL.User = url.UserPassword(cfg.Username, string(cfg.Password))
	}

	dsnURL.RawQuery = queryParams.Encode()

	return dsnURL.String(), nil
}

// shouldCreateSchema returns true if the exporter should run the DDL for creating database/tables.
func (cfg *Config) shouldCreateSchema() bool {
	return cfg.CreateSchema
}

func (cfg *Config) buildMetricTableNames() {
	tableName := defaultMetricTableName

	if cfg.MetricsTableName != "" && !cfg.areMetricTableNamesSet() {
		tableName = cfg.MetricsTableName
	}

	if cfg.MetricsTables.Gauge.Name == "" {
		cfg.MetricsTables.Gauge.Name = tableName + defaultGaugeSuffix
	}
	if cfg.MetricsTables.Sum.Name == "" {
		cfg.MetricsTables.Sum.Name = tableName + defaultSumSuffix
	}
	if cfg.MetricsTables.Summary.Name == "" {
		cfg.MetricsTables.Summary.Name = tableName + defaultSummarySuffix
	}
	if cfg.MetricsTables.Histogram.Name == "" {
		cfg.MetricsTables.Histogram.Name = tableName + defaultHistogramSuffix
	}
	if cfg.MetricsTables.ExponentialHistogram.Name == "" {
		cfg.MetricsTables.ExponentialHistogram.Name = tableName + defaultExpHistogramSuffix
	}
}

func (cfg *Config) areMetricTableNamesSet() bool {
	return cfg.MetricsTables.Gauge.Name != "" ||
		cfg.MetricsTables.Sum.Name != "" ||
		cfg.MetricsTables.Summary.Name != "" ||
		cfg.MetricsTables.Histogram.Name != "" ||
		cfg.MetricsTables.ExponentialHistogram.Name != ""
}

// tableEngineString generates the ENGINE string.
func (cfg *Config) tableEngineString() string {
	engine := cfg.TableEngine.Name
	params := cfg.TableEngine.Params

	if cfg.TableEngine.Name == "" {
		engine = defaultTableEngineName
		params = ""
	}

	return fmt.Sprintf("%s(%s)", engine, params)
}

// database returns the preferred database for creating tables and inserting data.
// The config option takes precedence over the DSN's settings.
// Falls back to default if neither are set.
// Assumes config has passed Validate.
func (cfg *Config) database() string {
	if cfg.Database != "" && cfg.Database != defaultDatabase {
		return cfg.Database
	}

	dsn, err := cfg.buildDSN()
	if err != nil {
		return ""
	}

	dsnDB, err := internal.DatabaseFromDSN(dsn)
	if err != nil {
		return ""
	}

	if dsnDB != "" && dsnDB != defaultDatabase {
		return dsnDB
	}

	return defaultDatabase
}

// clusterString generates the ON CLUSTER string. Returns empty string if not set.
func (cfg *Config) clusterString() string {
	if cfg.ClusterName == "" {
		return ""
	}

	return fmt.Sprintf("ON CLUSTER %s", cfg.ClusterName)
}
