package server

import (
	"database/sql"
	"encoding/json"
	"time"

	_ "modernc.org/sqlite"

	"github.com/iconidentify/maclab/internal/api"
)

type store struct{ db *sql.DB }

func openStore(path string) (*store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS devices (id TEXT PRIMARY KEY, name TEXT UNIQUE NOT NULL, secret_hash TEXT NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS tokens (token_hash TEXT PRIMARY KEY, name TEXT NOT NULL, label TEXT, model TEXT, expires INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY, device TEXT NOT NULL, created INTEGER NOT NULL, state TEXT NOT NULL, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS jobs_device ON jobs(device, created);
CREATE TABLE IF NOT EXISTS builds (id TEXT PRIMARY KEY, key TEXT NOT NULL, created INTEGER NOT NULL, state TEXT NOT NULL, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS builds_key ON builds(key, created);
CREATE TABLE IF NOT EXISTS maintainers (name TEXT PRIMARY KEY, token_hash TEXT UNIQUE NOT NULL, added INTEGER NOT NULL, added_by TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS configs (device TEXT NOT NULL, kernel TEXT NOT NULL, sha TEXT NOT NULL, PRIMARY KEY(device, kernel));`)
	if err != nil {
		return nil, err
	}
	return &store{db}, nil
}

func (s *store) saveDevice(d *api.Device, secretHash string) error {
	b, _ := json.Marshal(d)
	_, err := s.db.Exec(`INSERT INTO devices(id,name,secret_hash,data) VALUES(?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, secret_hash=excluded.secret_hash, data=excluded.data`, d.ID, d.Name, secretHash, string(b))
	return err
}

type deviceRow struct {
	d          api.Device
	secretHash string
}

func (s *store) devices() ([]deviceRow, error) {
	rows, err := s.db.Query(`SELECT secret_hash, data FROM devices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []deviceRow
	for rows.Next() {
		var r deviceRow
		var data string
		if err := rows.Scan(&r.secretHash, &data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &r.d); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *store) addToken(hash string, req api.EnrollTokenRequest, exp time.Time) error {
	_, err := s.db.Exec(`INSERT INTO tokens(token_hash,name,label,model,expires) VALUES(?,?,?,?,?)`, hash, req.Name, req.Label, req.Model, exp.Unix())
	return err
}

// token looks up an enrollment token that has not expired.
func (s *store) token(hash string, now time.Time) (*api.EnrollTokenRequest, error) {
	var r api.EnrollTokenRequest
	var exp int64
	err := s.db.QueryRow(`SELECT name,label,model,expires FROM tokens WHERE token_hash=?`, hash).Scan(&r.Name, &r.Label, &r.Model, &exp)
	if err != nil {
		return nil, err
	}
	if now.Unix() > exp {
		return nil, sql.ErrNoRows
	}
	return &r, nil
}

// spendTokens uses up the enrollment tokens for a name: its Mac has checked in.
func (s *store) spendTokens(name string) error {
	_, err := s.db.Exec(`DELETE FROM tokens WHERE name=?`, name)
	return err
}

func (s *store) saveJob(j *api.Job) error {
	b, _ := json.Marshal(j)
	_, err := s.db.Exec(`INSERT INTO jobs(id,device,created,state,data) VALUES(?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET state=excluded.state, data=excluded.data`, j.ID, j.Spec.Device, j.Created.UnixNano(), string(j.State), string(b))
	return err
}

func (s *store) job(id string) (*api.Job, error) {
	var data string
	if err := s.db.QueryRow(`SELECT data FROM jobs WHERE id=?`, id).Scan(&data); err != nil {
		return nil, err
	}
	var j api.Job
	return &j, json.Unmarshal([]byte(data), &j)
}

func (s *store) jobs(device string, limit int, unfinished bool) ([]*api.Job, error) {
	q := `SELECT data FROM jobs WHERE (?='' OR device=?)`
	if unfinished {
		q += ` AND state != 'done'`
	}
	q += ` ORDER BY created DESC LIMIT ?`
	rows, err := s.db.Query(q, device, device, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.Job
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var j api.Job
		if err := json.Unmarshal([]byte(data), &j); err != nil {
			return nil, err
		}
		out = append(out, &j)
	}
	return out, rows.Err()
}

func (s *store) saveBuild(b *api.Build) error {
	d, _ := json.Marshal(b)
	_, err := s.db.Exec(`INSERT INTO builds(id,key,created,state,data) VALUES(?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET state=excluded.state, data=excluded.data`, b.ID, b.Key, b.Created.UnixNano(), string(b.State), string(d))
	return err
}

func (s *store) builds(where string, args ...any) ([]*api.Build, error) {
	rows, err := s.db.Query(`SELECT data FROM builds WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.Build
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var b api.Build
		if err := json.Unmarshal([]byte(data), &b); err != nil {
			return nil, err
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

func (s *store) configFor(device, kernel string) string {
	var sha string
	s.db.QueryRow(`SELECT sha FROM configs WHERE device=? AND kernel=?`, device, kernel).Scan(&sha)
	return sha
}

func (s *store) setConfig(device, kernel, sha string) error {
	_, err := s.db.Exec(`INSERT INTO configs(device,kernel,sha) VALUES(?,?,?) ON CONFLICT(device,kernel) DO UPDATE SET sha=excluded.sha`, device, kernel, sha)
	return err
}

func (s *store) addMaintainer(m api.Maintainer, tokenHash string) error {
	_, err := s.db.Exec(`INSERT INTO maintainers(name,token_hash,added,added_by) VALUES(?,?,?,?)`, m.Name, tokenHash, m.Added.Unix(), m.AddedBy)
	return err
}

// maintainerByToken is the name of the Maintainer holding a token hash.
func (s *store) maintainerByToken(hash string) (string, error) {
	var name string
	err := s.db.QueryRow(`SELECT name FROM maintainers WHERE token_hash=?`, hash).Scan(&name)
	return name, err
}

func (s *store) hasMaintainer(name string) bool {
	var n int
	s.db.QueryRow(`SELECT count(*) FROM maintainers WHERE name=?`, name).Scan(&n)
	return n > 0
}

func (s *store) countMaintainers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM maintainers`).Scan(&n)
	return n, err
}

func (s *store) maintainers() ([]api.Maintainer, error) {
	rows, err := s.db.Query(`SELECT name,added,added_by FROM maintainers ORDER BY added, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Maintainer{}
	for rows.Next() {
		var m api.Maintainer
		var added int64
		if err := rows.Scan(&m.Name, &added, &m.AddedBy); err != nil {
			return nil, err
		}
		m.Added = time.Unix(added, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// removeMaintainer deletes a Maintainer, and with them their token, unless
// they are the last one: labd without a Maintainer can't be reached.
func (s *store) removeMaintainer(name string) (removed bool, err error) {
	res, err := s.db.Exec(`DELETE FROM maintainers WHERE name=? AND (SELECT count(*) FROM maintainers) > 1`, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
