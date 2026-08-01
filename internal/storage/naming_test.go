package storage

import (
	"strings"
	"testing"
)

func TestTableName(t *testing.T) {
	cases := []struct{ name, want string }{
		{"Users", "ddb_"},
		{"", "ddb_"},
		{"orders-2024.table", "ddb_"},
	}
	for _, c := range cases {
		got := TableName(c.name)
		if !strings.HasPrefix(got, "ddb_") {
			t.Errorf("TableName(%q) = %q, want ddb_ prefix", c.name, got)
		}
		if len(got) != len("ddb_")+16 {
			t.Errorf("TableName(%q) length = %d, want %d", c.name, len(got), len("ddb_")+16)
		}
	}
}

func TestTableNameDeterministic(t *testing.T) {
	if TableName("Users") != TableName("Users") {
		t.Error("TableName is not deterministic")
	}
	if TableName("a") == TableName("b") {
		t.Error("TableName collided for distinct inputs")
	}
}
