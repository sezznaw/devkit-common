package mysqlx

import (
	"errors"

	driver "github.com/go-sql-driver/mysql"
)

// IsCode says whether err is a MySQL server error with one of the codes
// (1062 duplicate entry, 1060 duplicate column, 1061 duplicate key name ...).
func IsCode(err error, codes ...uint16) bool {
	var me *driver.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	for _, c := range codes {
		if me.Number == c {
			return true
		}
	}
	return false
}
