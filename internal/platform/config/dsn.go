package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
)

// DSN renders the PostgreSQL connection URL, including the password.
//
// The result is a credential. It is passed straight to the pool constructor
// and must never be logged, embedded in an error, or returned from an API.
// Use [Database.SafeDSN] anywhere a human or a log line will see it.
func (d Database) DSN() string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(d.User, d.Password),
		Host:   net.JoinHostPort(d.Host, strconv.Itoa(d.Port)),
		Path:   "/" + d.Name,
	}
	q := url.Values{}
	q.Set("sslmode", d.SSLMode)
	q.Set("connect_timeout", strconv.Itoa(int(d.ConnectTimeout.Seconds())))
	// Tag the connection so `pg_stat_activity` attributes queries to Atlas.
	// Costs nothing and repays itself the first time a DBA asks who is
	// holding a connection.
	q.Set("application_name", "atlas")
	u.RawQuery = q.Encode()
	return u.String()
}

// SafeDSN renders the connection URL with the password replaced, for logs and
// diagnostics.
func (d Database) SafeDSN() string {
	safe := d
	safe.Password = "xxxxx"
	u, err := url.Parse(safe.DSN())
	if err != nil {
		return fmt.Sprintf("postgres://%s@%s:%d/%s", d.User, d.Host, d.Port, d.Name)
	}
	// url.String re-encodes the placeholder rather than the real secret.
	u.User = url.UserPassword(d.User, "xxxxx")
	return u.String()
}

// LogValue implements slog.LogValuer so that a Database value logged as an
// attribute renders without its password, whatever key it is logged under.
//
// The log package's key-based redaction catches an attribute literally named
// `password`; this catches the case where the whole struct is logged at once,
// which key matching cannot see into.
func (d Database) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("host", d.Host),
		slog.Int("port", d.Port),
		slog.String("name", d.Name),
		slog.String("user", d.User),
		slog.String("ssl_mode", d.SSLMode),
	)
}
