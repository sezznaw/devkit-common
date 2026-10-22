package mysqlx

import (
	"errors"
	"fmt"
	"testing"

	driver "github.com/go-sql-driver/mysql"
)

func TestIsCode(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &driver.MySQLError{Number: 1060, Message: "Duplicate column name 'service'"})
	if !IsCode(err, 1060, 1061) || IsCode(err, 1062) || IsCode(errors.New("x"), 1060) {
		t.Fatal("IsCode")
	}
}
