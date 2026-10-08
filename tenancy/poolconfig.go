package tenancy

import (
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Variables de entorno que dimensionan el pool de cada tenant. Cada réplica de
// cada core abre un pool por tenant, así que el total de conexiones del
// Postgres compartido es réplicas × cores × tenants × MaxConns: los defaults de
// pgx (max(4, NumCPU), 30 min ociosa) lo hacían crecer sin control.
const (
	EnvPoolMaxConns     = "STRIX_DB_POOL_MAX_CONNS"
	EnvPoolMinConns     = "STRIX_DB_POOL_MIN_CONNS"
	EnvPoolMaxIdle      = "STRIX_DB_POOL_MAX_IDLE"
	EnvPoolMaxLifetime  = "STRIX_DB_POOL_MAX_LIFETIME"
	EnvPoolHealthPeriod = "STRIX_DB_POOL_HEALTH_PERIOD"
	EnvPoolIdleClose    = "STRIX_DB_POOL_IDLE_CLOSE"
	EnvAppName          = "STRIX_APP_NAME"
	envServiceName      = "NATS_SERVICE_NAME"
)

// PoolSettings dimensiona los pools de tenant.
type PoolSettings struct {
	MaxConns     int32
	MinConns     int32
	MaxIdle      time.Duration
	MaxLifetime  time.Duration
	HealthPeriod time.Duration
	// IdleClose cierra el pool completo de un tenant al que ninguna petición
	// recurrió en ese tiempo. Cero lo desactiva.
	IdleClose time.Duration
	// AppName aparece como application_name en pg_stat_activity.
	AppName string
}

// DefaultPoolSettings son los valores cuando el entorno no dice otra cosa.
func DefaultPoolSettings() PoolSettings {
	return PoolSettings{
		MaxConns:     3,
		MinConns:     0,
		MaxIdle:      2 * time.Minute,
		MaxLifetime:  30 * time.Minute,
		HealthPeriod: time.Minute,
		IdleClose:    10 * time.Minute,
	}
}

// PoolSettingsFromEnv parte de los defaults y aplica los STRIX_DB_POOL_*. Un
// valor ausente o ilegible conserva el default.
func PoolSettingsFromEnv() PoolSettings {
	s := DefaultPoolSettings()
	if v, ok := envInt(EnvPoolMaxConns); ok && v >= 1 {
		s.MaxConns = int32(v)
	}
	if v, ok := envInt(EnvPoolMinConns); ok && v >= 0 {
		s.MinConns = int32(v)
	}
	if s.MinConns > s.MaxConns {
		s.MinConns = s.MaxConns
	}
	if v, ok := envDuration(EnvPoolMaxIdle); ok {
		s.MaxIdle = v
	}
	if v, ok := envDuration(EnvPoolMaxLifetime); ok {
		s.MaxLifetime = v
	}
	if v, ok := envDuration(EnvPoolHealthPeriod); ok && v > 0 {
		s.HealthPeriod = v
	}
	if v, ok := envDuration(EnvPoolIdleClose); ok {
		s.IdleClose = v
	}
	s.AppName = AppNameFromEnv()
	return s
}

// AppNameFromEnv devuelve STRIX_APP_NAME o, si falta, NATS_SERVICE_NAME.
func AppNameFromEnv() string {
	if v := os.Getenv(EnvAppName); v != "" {
		return v
	}
	return os.Getenv(envServiceName)
}

func (s PoolSettings) apply(cfg *pgxpool.Config) {
	cfg.MaxConns = s.MaxConns
	cfg.MinConns = s.MinConns
	cfg.MaxConnIdleTime = s.MaxIdle
	cfg.MaxConnLifetime = s.MaxLifetime
	cfg.HealthCheckPeriod = s.HealthPeriod
	if s.AppName != "" {
		if _, set := cfg.ConnConfig.RuntimeParams["application_name"]; !set {
			cfg.ConnConfig.RuntimeParams["application_name"] = s.AppName
		}
	}
}

func envInt(key string) (int, bool) {
	v := os.Getenv(key)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	return n, err == nil
}

func envDuration(key string) (time.Duration, bool) {
	v := os.Getenv(key)
	if v == "" {
		return 0, false
	}
	d, err := time.ParseDuration(v)
	return d, err == nil && d >= 0
}
