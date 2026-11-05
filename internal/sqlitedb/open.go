package sqlitedb

import (
	"database/sql"
	"net/url"

	_ "modernc.org/sqlite"
)

func Open(path string, readOnly bool) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
		q.Add("_pragma", "foreign_keys(1)")
		q.Set("_txlock", "immediate")
	}
	return sql.Open("sqlite", "file:"+path+"?"+q.Encode())
}
