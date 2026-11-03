// Package postgresql manages private native PostgreSQL instances and their
// observable, injectable protocol endpoints. It does not implement SQL.
package postgresql

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const Prefix = "/opt/interceptor/postgresql"
const Version = "18.6"
const VectorVersion = "0.8.7"

type Config struct {
	Database        string   `yaml:"database" json:"database"`
	Roles           []Role   `yaml:"roles" json:"roles"`
	Schema          []string `yaml:"schema" json:"schema"`
	Seeds           []string `yaml:"seeds" json:"seeds"`
	Tables          []Table  `yaml:"tables,omitempty" json:"tables,omitempty"`
	MaxConnections  int      `yaml:"max_connections,omitempty" json:"max_connections,omitempty"`
	QueryTimeoutMS  int      `yaml:"query_timeout_ms,omitempty" json:"query_timeout_ms,omitempty"`
	MaxResultBytes  int64    `yaml:"max_result_bytes,omitempty" json:"max_result_bytes,omitempty"`
	MaxStorageBytes int64    `yaml:"max_storage_bytes,omitempty" json:"max_storage_bytes,omitempty"`
}
type Role struct {
	Name     string `yaml:"name" json:"name"`
	Password string `yaml:"password" json:"password"`
}
type Table struct {
	ID          string   `yaml:"id" json:"id"`
	Schema      string   `yaml:"schema" json:"schema"`
	Name        string   `yaml:"name" json:"name"`
	Keys        []string `yaml:"keys" json:"keys"`
	Fields      []string `yaml:"fields" json:"fields"`
	AllowCreate bool     `yaml:"allow_create,omitempty" json:"allow_create,omitempty"`
	MaxRecords  int      `yaml:"max_records,omitempty" json:"max_records,omitempty"`
}

var identifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,62}$`)

func (c *Config) Defaults() {
	if c.MaxConnections == 0 {
		c.MaxConnections = 16
	}
	if c.QueryTimeoutMS == 0 {
		c.QueryTimeoutMS = 30000
	}
	if c.MaxResultBytes == 0 {
		c.MaxResultBytes = 8 << 20
	}
	if c.MaxStorageBytes == 0 {
		c.MaxStorageBytes = 256 << 20
	}
}
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("postgresql configuration is required")
	}
	if !identifier.MatchString(c.Database) || strings.HasPrefix(c.Database, "pg_") || c.Database == "postgres" || strings.HasPrefix(c.Database, "template") {
		return errors.New("invalid application database name")
	}
	if len(c.Roles) == 0 || len(c.Roles) > 16 || len(c.Schema) == 0 || len(c.Schema)+len(c.Seeds) > 128 {
		return errors.New("postgresql requires roles and schema assets within configured ceilings")
	}
	if c.MaxConnections < 1 || c.MaxConnections > 128 || c.QueryTimeoutMS < 100 || c.QueryTimeoutMS > 120000 || c.MaxResultBytes < 1024 || c.MaxResultBytes > 64<<20 || c.MaxStorageBytes < 32<<20 || c.MaxStorageBytes > 10<<30 {
		return errors.New("postgresql limits are outside supported bounds")
	}
	seen := map[string]bool{}
	for _, r := range c.Roles {
		if !identifier.MatchString(r.Name) || strings.HasPrefix(r.Name, "pg_") || strings.HasPrefix(r.Name, "interceptor_") || r.Name == "postgres" || seen[r.Name] || len(r.Password) < 8 || len(r.Password) > 256 || strings.ContainsRune(r.Password, 0) {
			return errors.New("invalid or duplicate PostgreSQL application role")
		}
		seen[r.Name] = true
	}
	seen = map[string]bool{}
	for _, t := range c.Tables {
		if !identifier.MatchString(t.ID) || !identifier.MatchString(t.Schema) || !identifier.MatchString(t.Name) || seen[t.ID] || len(t.Keys) == 0 || len(t.Keys) > 8 || len(t.Fields) == 0 || len(t.Fields) > 64 {
			return errors.New("invalid or duplicate PostgreSQL injection table")
		}
		seen[t.ID] = true
		fields := map[string]bool{}
		for _, name := range append(append([]string{}, t.Keys...), t.Fields...) {
			if !identifier.MatchString(name) || fields[name] {
				return fmt.Errorf("invalid or duplicate key/field %q", name)
			}
			fields[name] = true
		}
		if t.AllowCreate && (t.MaxRecords < 1 || t.MaxRecords > 100000) {
			return errors.New("creation-enabled database table requires max_records between 1 and 100000")
		}
	}
	return nil
}
