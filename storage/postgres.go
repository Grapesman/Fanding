package storage

import "fmt"

type Postgres struct{}

func OpenPostgres(dsn string) (*Postgres, error) {
	return nil, fmt.Errorf("postgres driver is not enabled in this portable archive; memory store is used for now")
}
func (p *Postgres) Close() error   { return nil }
func (p *Postgres) Migrate() error { return nil }
