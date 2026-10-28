package main

import (
	"flag"
	"io"
	"slices"
	"testing"
)

func TestParseInterspersed(t *testing.T) {
	for _, args := range [][]string{{"admin", "-db", "x.db"}, {"-db", "x.db", "admin"}} {
		fs := flag.NewFlagSet("user", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		db := fs.String("db", "", "")
		names, err := parseInterspersed(fs, args)
		if err != nil || *db != "x.db" || !slices.Equal(names, []string{"admin"}) {
			t.Errorf("%v: names=%v db=%q err=%v", args, names, *db, err)
		}
	}
}
