package kitexx

import "testing"

func TestNormalizeTypeAndNarrows(t *testing.T) {
	norm := map[string]string{
		"VARCHAR(64)":      "varchar(64)",
		"bigint(20)":       "bigint",
		"int(11) unsigned": "int unsigned",
		"BIGINT UNSIGNED":  "bigint unsigned",
		"tinyint(1)":       "tinyint(1)",
		"tinyint(4)":       "tinyint",
		"bool":             "tinyint(1)",
		"decimal(24, 3)":   "decimal(24,3)",
		"datetime(3)":      "datetime(3)",
		" json ":           "json",
		"varchar(64) ":     "varchar(64)",
	}
	for in, want := range norm {
		if got := normalizeType(in); got != want {
			t.Errorf("normalizeType(%q) = %q, want %q", in, got, want)
		}
	}
	narrow := map[[2]string]bool{
		{"varchar(64)", "varchar(32)"}:     true,
		{"varchar(32)", "varchar(64)"}:     false,
		{"char(3)", "char(2)"}:             true,
		{"bigint", "int"}:                  true,
		{"int", "bigint"}:                  false,
		{"bigint", "bigint unsigned"}:      true,
		{"bigint unsigned", "bigint"}:      false,
		{"tinyint", "tinyint(1)"}:          false,
		{"decimal(24,3)", "decimal(24,4)"}: false,
		{"decimal(24,4)", "decimal(24,3)"}: true,
		{"decimal(24,3)", "decimal(20,3)"}: true,
		{"text", "longtext"}:               false,
		{"longtext", "text"}:               true,
		{"text", "varchar(4000)"}:          true,
		{"varchar(64)", "text"}:            false,
		{"datetime", "datetime(3)"}:        false,
		{"varchar(64)", "json"}:            false,
	}
	for pair, want := range narrow {
		if got := narrows(pair[0], pair[1]); got != want {
			t.Errorf("narrows(%q -> %q) = %v, want %v", pair[0], pair[1], got, want)
		}
	}
}

func TestSplitCollate(t *testing.T) {
	for in, want := range map[string][2]string{
		"varchar(128) COLLATE utf8mb4_0900_bin":                      {"varchar(128)", "utf8mb4_0900_bin"},
		"varchar(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin": {"varchar(64)", "utf8mb4_0900_bin"},
		"varchar(64) charset utf8mb4 collate utf8mb4_bin":            {"varchar(64)", "utf8mb4_bin"},
		"decimal(24,3)":   {"decimal(24,3)", ""},
		"bigint unsigned": {"bigint unsigned", ""},
	} {
		typ, coll := splitCollate(in)
		if typ != want[0] || coll != want[1] {
			t.Errorf("%q: got %q %q, want %q %q", in, typ, coll, want[0], want[1])
		}
	}
}
